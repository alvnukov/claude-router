package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type routerServer struct {
	cfg        config
	life       *lifecycle
	health     *health
	state      string
	cs         *configStore
	st         *store
	ui         *uiServer
	apiHTTP    *http.Server
	uiHTTP     *http.Server
	background context.Context
	cancel     context.CancelFunc
	once       sync.Once
}

func newRouterServer(cfg config, life *lifecycle, state string) *routerServer {
	cs := newConfigStore(cfg, providersPath())
	st := newStore(cfg.uiHistory, historyPath())
	st.life = life
	codexAuth.life = life
	h := newHealth(healthPath())
	h.life = life
	if life.mode() != modeStandby {
		if err := h.loadSnapshot(slotStatePath(state)); err != nil {
			log.Printf("slot snapshot: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &routerServer{cfg: cfg, life: life, health: h, state: state, cs: cs, st: st, background: ctx, cancel: cancel}
	r.ui = newUIServer(st, cs, h)
	r.ui.life = life
	return r
}

func slotStatePath(path string) string {
	if slot := os.Getenv("ROUTER_SLOT"); slot != "" {
		return filepath.Join(filepath.Dir(path), "state."+slot+".json")
	}
	return ""
}

func (r *routerServer) startBackground() {
	r.once.Do(func() {
		r.cs.watch(r.background, 2*time.Second, r.life)
		startChecker(r.background, r.cs, r.health, r.life)
		r.ui.startCatalogUpdates(r.background)
	})
}

func (r *routerServer) serve(api, ui net.Listener) error {
	if r.life.mode() == modeActive {
		r.startBackground()
	}
	apiHandler := newRouterHandler(r.cfg, r.cs, r.st, r.health, r.life)
	admin := newRuntimeAdmin(r.life, r.health, r.state)
	admin.activate = func() error {
		if r.life.mode() == modeStandby {
			if err := r.cs.reloadProviders(); err != nil {
				return err
			}
		}
		r.startBackground()
		return nil
	}
	mux := http.NewServeMux()
	mux.Handle("/admin/", admin)
	mux.Handle("/", apiHandler)
	r.apiHTTP = &http.Server{Handler: mux}
	errors := make(chan error, 2)
	go func() { errors <- r.apiHTTP.Serve(api) }()
	if ui != nil {
		r.uiHTTP = &http.Server{Handler: r.ui.handler()}
		go func() { errors <- r.uiHTTP.Serve(ui) }()
	}
	for i := 0; i < 1+boolInt(ui != nil); i++ {
		if err := <-errors; err != nil && err != http.ErrServerClosed {
			return err
		}
	}
	return nil
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (r *routerServer) shutdown(ctx context.Context) error {
	r.life.drain()
	r.cancel()
	if r.apiHTTP != nil {
		if err := r.apiHTTP.Shutdown(ctx); err != nil {
			return err
		}
	}
	if r.uiHTTP != nil {
		if err := r.uiHTTP.Shutdown(ctx); err != nil {
			return err
		}
	}
	return r.life.shutdown(ctx, r.health, slotStatePath(r.state))
}

func (r *routerServer) run() error {
	api, err := net.Listen("tcp", r.cfg.listen)
	if err != nil {
		return err
	}
	var ui net.Listener
	if r.cfg.uiListen != "" {
		ui, err = net.Listen("tcp", r.cfg.uiListen)
		if err != nil {
			api.Close()
			return err
		}
	}
	log.Printf("listening on %s, ui %s, mode %s", r.cfg.listen, r.cfg.uiListen, r.life.mode())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { done <- r.serve(api, ui) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
	}
	timeout := time.Duration(atoiOr(env("ROUTER_DRAIN_TIMEOUT", "900"), 900)) * time.Second
	if timeout <= 0 {
		return fmt.Errorf("ROUTER_DRAIN_TIMEOUT must be positive")
	}
	wait, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := r.shutdown(wait); err != nil {
		return err
	}
	return <-done
}
