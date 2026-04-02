package failover

import (
	"net/http"
	"net/url"
	"testing"
)

func TestBuildTargetURLPrefersChannelResolvePath(t *testing.T) {
	p := &Proxy{}
	ch := &Channel{
		BaseURL: "https://example.com",
		ResolvePath: func(path string, r *http.Request) string {
			return "/resolved?beta=true"
		},
	}
	r := &http.Request{
		URL: &url.URL{
			Path:     "/v1/messages",
			RawQuery: "foo=bar",
		},
	}

	targetURL := p.buildTargetURL(r, ch)
	if targetURL != "https://example.com/resolved?beta=true&foo=bar" {
		t.Fatalf("unexpected target url: %s", targetURL)
	}
}

func TestBuildTargetURLUsesOriginalPathWhenNoResolvePath(t *testing.T) {
	p := &Proxy{}
	ch := &Channel{
		BaseURL: "https://example.com",
	}
	r := &http.Request{
		URL: &url.URL{
			Path:     "/v1/messages",
			RawQuery: "y=2",
		},
	}

	targetURL := p.buildTargetURL(r, ch)
	if targetURL != "https://example.com/v1/messages?y=2" {
		t.Fatalf("unexpected target url: %s", targetURL)
	}
}

func TestBuildTargetURLPathQueryOverridesRequestQuery(t *testing.T) {
	p := &Proxy{}
	ch := &Channel{
		BaseURL: "https://example.com",
		ResolvePath: func(path string, r *http.Request) string {
			return "/v1/messages?beta=true"
		},
	}
	r := &http.Request{
		URL: &url.URL{
			Path:     "/v1/messages",
			RawQuery: "beta=false&foo=bar",
		},
	}

	targetURL := p.buildTargetURL(r, ch)
	if targetURL != "https://example.com/v1/messages?beta=true&foo=bar" {
		t.Fatalf("unexpected target url: %s", targetURL)
	}
}
