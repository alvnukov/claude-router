// Package config owns the router's configuration files: providers.json with
// its routing profiles, and the env file. Store serves the live snapshot and
// is the only writer of those files.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"
)

type Config struct {
	Listen        string
	PublicListen  string // where clients reach the router; a slot listens behind Caddy
	Upstream      *url.URL
	Local         Local
	MaxInputChars int
	Failover      bool
	FirstByte     time.Duration // give up on a model that has not answered by then
	Balance       int           // spread requests over this many best-rated models; <2 sends everything to the first
	ProbeEvery    time.Duration // ping idle models this often; 0 disables
	PoolType      string        // PoolFailover or PoolBalance for a pool route: pool order, no rating; "" keeps the rating order
	PoolName      string        // the pool a pool route resolved to; "" otherwise

	UIListen  string
	UIHistory int
}

// MarshalJSON refuses: Local carries the providers' API keys, and a JSON
// answer built from the whole Config would send them out.
func (Config) MarshalJSON() ([]byte, error) {
	return nil, errors.New("config.Config не кодируется в JSON")
}

// Whole seconds, as pool settings store them.
func (c Config) FirstByteSec() int { return int(c.FirstByte / time.Second) }
func (c Config) ProbeSec() int     { return int(c.ProbeEvery / time.Second) }

// ClientListen is the address clients should use to reach the router.
func (c Config) ClientListen() string {
	if c.PublicListen != "" {
		return c.PublicListen
	}
	return c.Listen
}

// MigrateConfig brings providers.json at path to the current schema, keeping
// a backup of each step.
func MigrateConfig(c Config, path string) (Config, error) {
	if migrated, changed := migrateLegacyPools(c.Local, SplitList(os.Getenv("ROUTER_CLOUD_ONLY"))); changed {
		if err := savePoolMigration(path, migrated); err != nil {
			return Config{}, fmt.Errorf("pool migration: %w", err)
		}
		c.Local = migrated
	}
	if migrated, changed := migrateFamilyRoutes(c.Local); changed {
		if err := SaveConfigurationMigration(path, migrated, ".before-families"); err != nil {
			return Config{}, fmt.Errorf("family migration: %w", err)
		}
		c.Local = migrated
	}
	if migrated, changed := migratePoolSettings(c); changed {
		if err := SaveConfigurationMigration(path, migrated, ".before-pool-settings"); err != nil {
			return Config{}, fmt.Errorf("pool settings migration: %w", err)
		}
		c.Local = migrated
	}
	return c, nil
}

// Only explicit routes can serve a model. Unknown and disabled models never
// fall through to Anthropic or to another model's pool.
func (c Config) RouteFor(model, effort string) Route { return c.Local.RouteFor(model, effort) }

func (c Config) ForModel(model, effort string) Config {
	if effort == "" {
		effort = "default"
	}
	route := c.RouteFor(model, effort)
	next := c
	next.Local = c.Local.Clone()
	next.Local.Models = nil
	next.Local.Preferred = ""
	var targets []PoolTarget
	switch route.Mode {
	case "model":
		targets = []PoolTarget{{Model: route.Model, Effort: route.Effort}}
		next.Failover = false
	case "pool":
		targets = c.Local.ModelPools[route.Pool]
		next.PoolName, next.PoolType = route.Pool, PoolFailover
		if settings, ok := c.Local.PoolSettings[route.Pool]; ok {
			next = settings.Apply(next)
			if settings.Type == PoolBalance {
				next.PoolType = PoolBalance
			}
		}
	default:
		return next
	}
	if len(targets) == 0 {
		return next
	}
	next.Local.Preferred = targets[0].Model
	for _, target := range targets {
		for _, m := range c.Local.Models {
			if m.Key() == target.Model {
				m.Efforts = map[string]string{effort: target.Effort}
				next.Local.Models = append(next.Local.Models, m)
				break
			}
		}
	}
	return next
}

func SplitList(s string) []string {
	return strings.FieldsFunc(s, func(c rune) bool { return c == ',' || c == '\n' || c == ' ' })
}
