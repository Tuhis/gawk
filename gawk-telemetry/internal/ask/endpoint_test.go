package ask

import "testing"

func TestEndpoint(t *testing.T) {
	for in, want := range map[string]struct {
		host     string
		external bool
	}{
		"https://api.anthropic.com":                    {"api.anthropic.com", true},
		"http://ollama:11434":                          {"ollama", false},
		"http://ollama.ollama.svc:11434/v1":            {"ollama.ollama.svc", false},
		"http://ollama.ollama.svc.cluster.local:11434": {"ollama.ollama.svc.cluster.local", false},
		"http://127.0.0.1:8080":                        {"127.0.0.1", false},
		"http://192.168.1.20:11434":                    {"192.168.1.20", false},
		"http://[::1]:11434":                           {"::1", false},
		"https://llm.example.com":                      {"llm.example.com", true},
		"http://203.0.113.9:11434":                     {"203.0.113.9", true},
		"http://localhost:11434":                       {"localhost", false},
	} {
		host, ext := Endpoint(in)
		if host != want.host || ext != want.external {
			t.Errorf("Endpoint(%q) = %q, %v; want %q, %v", in, host, ext, want.host, want.external)
		}
	}
}
