package upstreamproxy

import "testing"

const scutilSample = `<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
    1 : 169.254/16
  }
  FTPPassive : 1
  HTTPEnable : 1
  HTTPPort : 7890
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 7890
  HTTPSProxy : 127.0.0.1
  SOCKSEnable : 0
}
`

func TestParseScutilProxy(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string // "" = nil
	}{
		{name: "https wins", output: scutilSample, want: "http://127.0.0.1:7890"},
		{
			name:   "http only",
			output: "HTTPEnable : 1\nHTTPProxy : 10.0.0.1\nHTTPPort : 3128\n",
			want:   "http://10.0.0.1:3128",
		},
		{
			name:   "socks fallback",
			output: "HTTPEnable : 0\nSOCKSEnable : 1\nSOCKSProxy : 10.0.0.2\nSOCKSPort : 1080\n",
			want:   "socks5://10.0.0.2:1080",
		},
		{
			name:   "disabled",
			output: "HTTPEnable : 0\nHTTPSEnable : 0\nSOCKSEnable : 0\n",
			want:   "",
		},
		{
			name:   "pac only is ignored",
			output: "ProxyAutoConfigEnable : 1\nProxyAutoConfigURLString : http://wpad/proxy.pac\n",
			want:   "",
		},
		{
			name:   "enabled but no host",
			output: "HTTPEnable : 1\nHTTPPort : 8080\n",
			want:   "",
		},
		{
			name:   "garbage",
			output: "not scutil output at all",
			want:   "",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			u := parseScutilProxy(test.output)
			got := ""
			if u != nil {
				got = u.String()
			}
			if got != test.want {
				t.Fatalf("parseScutilProxy() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestParseWindowsProxyServer(t *testing.T) {
	tests := []struct {
		value string
		want  string
	}{
		{value: "127.0.0.1:7890", want: "http://127.0.0.1:7890"},
		{value: "http=10.0.0.1:3128;https=10.0.0.2:8443", want: "http://10.0.0.2:8443"},
		{value: "http=10.0.0.1:3128;socks=10.0.0.3:1080", want: "http://10.0.0.1:3128"},
		{value: "socks=10.0.0.3:1080", want: "socks5://10.0.0.3:1080"},
		{value: "", want: ""},
		{value: "  ", want: ""},
		{value: "foo;bar", want: ""},
	}
	for _, test := range tests {
		u := parseWindowsProxyServer(test.value)
		got := ""
		if u != nil {
			got = u.String()
		}
		if got != test.want {
			t.Errorf("parseWindowsProxyServer(%q) = %q, want %q", test.value, got, test.want)
		}
	}
}

func TestParseGnomeProxy(t *testing.T) {
	tests := []struct {
		mode string
		host string
		port int
		want string
	}{
		{mode: "manual", host: "127.0.0.1", port: 7890, want: "http://127.0.0.1:7890"},
		{mode: "manual", host: "127.0.0.1", port: 0, want: "http://127.0.0.1:8080"},
		{mode: "auto", host: "127.0.0.1", port: 7890, want: ""},
		{mode: "none", host: "127.0.0.1", port: 7890, want: ""},
		{mode: "manual", host: "", port: 7890, want: ""},
	}
	for _, test := range tests {
		u := parseGnomeProxy(test.mode, test.host, test.port)
		got := ""
		if u != nil {
			got = u.String()
		}
		if got != test.want {
			t.Errorf("parseGnomeProxy(%q, %q, %d) = %q, want %q", test.mode, test.host, test.port, got, test.want)
		}
	}
}
