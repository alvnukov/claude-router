//go:build !router_codex_loopback

package codextesttransport

import "net/http"

func Transport() http.RoundTripper { return nil }

func AuthTransport() http.RoundTripper { return nil }

func DisableBackground() bool { return false }
