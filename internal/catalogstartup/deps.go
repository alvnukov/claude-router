package catalogstartup

import (
	"errors"
	"net/http"
	"time"
)

// Dependencies keeps catalog network access separate from request routing.
type Dependencies struct {
	officialURL string
	codexURL    string
	transport   *http.Transport
	guard       http.RoundTripper
}

func Production(officialURL, codexBaseURL string) Dependencies {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		transport = &http.Transport{Proxy: http.ProxyFromEnvironment}
	}
	return Dependencies{
		officialURL: officialURL,
		codexURL:    codexBaseURL + "/models?client_version=0.156.0",
		transport:   transport.Clone(),
	}
}

func (d Dependencies) OfficialURL() string    { return d.officialURL }
func (d Dependencies) CodexModelsURL() string { return d.codexURL }
func (d Dependencies) IsSynthetic() bool      { return d.guard != nil }

func (d Dependencies) Client(timeout time.Duration) *http.Client {
	client := &http.Client{Timeout: timeout, Transport: d.transport}
	if d.guard != nil {
		client.Transport = d.guard
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return errors.New("catalog synthetic redirect rejected")
		}
	}
	return client
}

func (d Dependencies) CodexClient(timeout time.Duration) *http.Client {
	client := d.Client(timeout)
	if d.guard == nil {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	return client
}

func (d Dependencies) SyntheticAuthClient(timeout time.Duration) *http.Client {
	if d.guard == nil {
		return nil
	}
	return d.Client(timeout)
}

func (d Dependencies) Close() {
	if d.transport != nil {
		d.transport.CloseIdleConnections()
	}
}
