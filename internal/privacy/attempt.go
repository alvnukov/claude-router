package privacy

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"localrouter/internal/anthropicerror"
	"localrouter/internal/providers/codex"
)

type attemptContextKey struct{}

type Attempt struct {
	mu           sync.Mutex
	policy       *Policy
	lifecycle    *protectedLifecycle
	exchange     *Exchange
	pool         string
	protocol     string
	failure      anthropicerror.Failure
	hasFailure   bool
	maskExpected bool
	prepared     bool
	rejected     bool
}

func NewAttempt(policy *Policy) *Attempt { return &Attempt{policy: policy} }

func (a *Attempt) SetPool(profile, pool string) {
	if profile != "" && pool != "" {
		a.pool = profile + "/" + pool
	}
}

func (a *Attempt) Pool() string { return a.pool }
func (a *Attempt) Protocol() string {
	if a == nil {
		return ""
	}
	return a.protocol
}

func (a *Attempt) Prepare(target Target, body []byte) ([]byte, error) {
	a.CloseExchange()
	a.prepared = false
	a.mu.Lock()
	a.failure, a.hasFailure = anthropicerror.Failure{}, false
	a.mu.Unlock()
	a.maskExpected = a.policy.config.Resolve(target).Enabled && a.policy.config.Resolve(target).Mode == ModeMask
	x, wire, err := a.policy.Prepare(target, body)
	a.exchange = x
	a.prepared = err == nil
	a.rejected = err != nil
	a.protocol = ""
	if a.prepared {
		a.protocol = target.Protocol
	}
	return wire, err
}

func (a *Attempt) Prepared() bool { return a != nil && a.prepared }
func (a *Attempt) Failed() bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hasFailure
}
func (a *Attempt) HasExchange() bool  { return a != nil && a.exchange != nil }
func (a *Attempt) MaskExpected() bool { return a != nil && a.maskExpected }

func (a *Attempt) CheckControl(value string) error {
	if a == nil || !a.prepared {
		return errTraffic
	}
	if a.exchange == nil {
		return nil
	}
	return a.exchange.CheckControl(value)
}

func (a *Attempt) Restore(body []byte, stream bool) ([]byte, error) {
	if a.exchange == nil {
		return nil, errRestore
	}
	return a.exchange.Restore(body, stream)
}

func (a *Attempt) CloseExchange() {
	if a != nil {
		a.exchange.Close()
		a.exchange = nil
	}
}

func (a *Attempt) Close() { a.CloseExchange() }

func WithAttempt(r *http.Request, a *Attempt) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), attemptContextKey{}, a))
}

func FromRequest(r *http.Request) *Attempt {
	if r == nil {
		return nil
	}
	a, _ := r.Context().Value(attemptContextKey{}).(*Attempt)
	return a
}

// ObserveProviderHeaders marks real local provider headers; generated Anthropic
// headers must not postpone an idle deadline already started here.
func ObserveProviderHeaders(r *http.Request) {
	if a := FromRequest(r); a != nil && a.lifecycle != nil {
		a.lifecycle.gotHeaders(nil)
	}
}

// ScrubLocalFailure captures only the current candidate's safe classification
// before its provider error can be replaced for protected history or logging.
func ScrubLocalFailure(r *http.Request, err error, detail string, status int) (string, error) {
	a := FromRequest(r)
	if a == nil {
		return "upstream response failed", err
	}
	a.mu.Lock()
	a.failure, a.hasFailure = anthropicerror.Failure{}, false
	if err != nil {
		a.failure = codex.ClassifyFailure(err, status)
		a.hasFailure = true
	}
	a.mu.Unlock()
	if err == nil {
		return "", nil
	}
	return "upstream response failed", errors.New("upstream response failed")
}

func FailureForResponse(r *http.Request, err error, status int) anthropicerror.Failure {
	f := codex.ClassifyFailure(err, status)
	if a := FromRequest(r); a != nil {
		a.mu.Lock()
		if a.hasFailure && a.failure.Status == status {
			f = a.failure
		}
		a.mu.Unlock()
	}
	return f
}
