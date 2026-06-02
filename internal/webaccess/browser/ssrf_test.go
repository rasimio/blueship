package browser

import "testing"

func TestValidateFetchURL(t *testing.T) {
	blocked := []string{
		"file:///etc/passwd",
		"file:///Users/someone/.env",
		"chrome://version",
		"data:text/html,<h1>x</h1>",
		"ftp://example.com/x",
		"http://localhost:13101",
		"https://localhost/admin",
		"http://api.localhost/x",
		"http://127.0.0.1:13101",
		"http://[::1]:8200",
		"http://169.254.169.254/latest/meta-data/", // cloud metadata
		"http://10.0.0.5/internal",
		"http://192.168.1.1/router",
		"http://172.16.0.9/x",
		"http://0.0.0.0:8080",
		"://nohost",
	}
	for _, u := range blocked {
		if err := validateFetchURL(u); err == nil {
			t.Errorf("expected %q to be rejected, but it passed", u)
		}
	}

	allowed := []string{
		"https://arxiv.org/abs/2401.00001",
		"http://example.com/page",
		"https://www.google.com/search?q=x",
		"https://news.ycombinator.com",
		"https://8.8.8.8/", // public IP literal is fine
	}
	for _, u := range allowed {
		if err := validateFetchURL(u); err != nil {
			t.Errorf("expected %q to be allowed, got: %v", u, err)
		}
	}
}
