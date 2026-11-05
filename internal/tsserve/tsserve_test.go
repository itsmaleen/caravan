package tsserve

import "testing"

// realStatus is verbatim `tailscale serve status --json` output captured from a
// real machine on 2026-07-29. Using the real shape as the fixture is the point:
// the parser has to survive what tailscaled actually emits, not what we assume.
const realStatus = `{
  "TCP": {
    "10000": { "HTTPS": true },
    "443": { "HTTPS": true },
    "8443": { "HTTPS": true }
  },
  "Web": {
    "ms-mac-mini.tail901772.ts.net:10000": {
      "Handlers": { "/": { "Proxy": "https+insecure://localhost:5173" } }
    },
    "ms-mac-mini.tail901772.ts.net:443": {
      "Handlers": {
        "/": { "Proxy": "https+insecure://127.0.0.1:5173" },
        "/wrinkles": { "Proxy": "http://127.0.0.1:18790" },
        "/wrinkles-api": { "Proxy": "http://127.0.0.1:18795" }
      }
    },
    "ms-mac-mini.tail901772.ts.net:8443": {
      "Handlers": { "/": { "Proxy": "http://127.0.0.1:80" } }
    }
  },
  "AllowFunnel": {
    "ms-mac-mini.tail901772.ts.net:10000": true,
    "ms-mac-mini.tail901772.ts.net:443": true
  }
}`

func TestParseStatus_RealOutput(t *testing.T) {
	st, err := ParseStatus([]byte(realStatus))
	if err != nil {
		t.Fatalf("ParseStatus: %v", err)
	}
	mounts := st.Mounts()
	if len(mounts) != 5 {
		t.Fatalf("want 5 mounts, got %d: %+v", len(mounts), mounts)
	}
}

func TestMounts_FunnelFlagFollowsAllowFunnel(t *testing.T) {
	st, _ := ParseStatus([]byte(realStatus))
	got := map[string]bool{}
	for _, m := range st.Mounts() {
		got[m.HostPort+m.Path] = m.Funnel
	}
	cases := map[string]bool{
		"ms-mac-mini.tail901772.ts.net:443/":         true, // funnelled
		"ms-mac-mini.tail901772.ts.net:443/wrinkles": true, // same host:port ⇒ also public
		"ms-mac-mini.tail901772.ts.net:10000/":       true,
		"ms-mac-mini.tail901772.ts.net:8443/":        false, // tailnet only
	}
	for key, want := range cases {
		if got[key] != want {
			t.Errorf("%s: funnel=%v, want %v", key, got[key], want)
		}
	}
}

func TestMountPort(t *testing.T) {
	cases := []struct {
		proxy string
		want  int
	}{
		{"http://127.0.0.1:18790", 18790},
		{"https+insecure://localhost:5173", 5173},
		{"https+insecure://127.0.0.1:5173", 5173},
		{"http://127.0.0.1:80", 80},
		{"http://127.0.0.1:8080/sub/path", 8080},
		{"http://127.0.0.1", 0},
		{"", 0},
	}
	for _, c := range cases {
		if got := (Mount{Proxy: c.proxy}).Port(); got != c.want {
			t.Errorf("Port(%q) = %d, want %d", c.proxy, got, c.want)
		}
	}
}

func TestMountURL_DropsImplicit443(t *testing.T) {
	m := Mount{HostPort: "host.ts.net:443", Path: "/wrinkles"}
	if got := m.URL(); got != "https://host.ts.net/wrinkles" {
		t.Errorf("URL() = %q", got)
	}
	root := Mount{HostPort: "host.ts.net:443", Path: "/"}
	if got := root.URL(); got != "https://host.ts.net" {
		t.Errorf("root URL() = %q", got)
	}
	alt := Mount{HostPort: "host.ts.net:10000", Path: "/"}
	if got := alt.URL(); got != "https://host.ts.net:10000" {
		t.Errorf("alt URL() = %q", got)
	}
}

func TestVisibility(t *testing.T) {
	if got := (Mount{Funnel: true}).Visibility(); got != "public" {
		t.Errorf("funnel visibility = %q", got)
	}
	if got := (Mount{}).Visibility(); got != "tailnet" {
		t.Errorf("serve visibility = %q", got)
	}
}

func TestParseStatus_NonJSONIsEmptyNotError(t *testing.T) {
	// A node serving nothing prints a human sentence, not JSON.
	st, err := ParseStatus([]byte("No serve config\n"))
	if err != nil {
		t.Fatalf("want nil error for non-JSON, got %v", err)
	}
	if len(st.Mounts()) != 0 {
		t.Errorf("want no mounts, got %+v", st.Mounts())
	}
}

func TestSetArgs_FunnelOnlyForPublic(t *testing.T) {
	pub := SetArgs("public", "/app", "http://127.0.0.1:3000")
	if pub[0] != "funnel" {
		t.Errorf("public should use funnel, got %v", pub)
	}
	tn := SetArgs("tailnet", "/app", "http://127.0.0.1:3000")
	if tn[0] != "serve" {
		t.Errorf("tailnet should use serve, got %v", tn)
	}
	want := []string{"serve", "--bg", "--set-path=/app", "http://127.0.0.1:3000"}
	for i := range want {
		if tn[i] != want[i] {
			t.Fatalf("SetArgs = %v, want %v", tn, want)
		}
	}
}
