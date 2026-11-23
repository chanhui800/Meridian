package main

import (
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestPathTargetCookieAliasesRemainTargetScoped(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			http.SetCookie(w, &http.Cookie{Name: "origin_session", Value: "alice", Path: "/", HttpOnly: true})
			return
		}
		cookie, err := r.Cookie("origin_session")
		if err != nil || cookie.Value != "alice" {
			http.Error(w, "session missing", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(origin.Close)
	otherCookies := make(chan string, 2)
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherCookies <- r.Header.Get("Cookie")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(other.Close)
	firstURL, _ := url.Parse(origin.URL)
	otherURL, _ := url.Parse(other.URL)
	gateway := newPathTargetRegressionGateway(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	response, err := client.Get(gateway.URL + "/http://" + firstURL.Host + "/login")
	if err != nil {
		t.Fatal(err)
	}
	drainProxyRegressionResponse(t, response)
	for _, prefix := range []string{"/http://", "/_proxy/v1/http/"} {
		response, err = client.Get(gateway.URL + prefix + otherURL.Host + "/protected")
		if err != nil {
			t.Fatal(err)
		}
		drainProxyRegressionResponse(t, response)
		if received := <-otherCookies; received != "" {
			t.Fatalf("origin cookie reached another target: %q", received)
		}
	}
}

func TestPathTargetDefaultCookiePathAndDeletionMatchBothFormats(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/account/login":
			http.SetCookie(w, &http.Cookie{Name: "origin_session", Value: "alice"})
			return
		case "/account/logout":
			http.SetCookie(w, &http.Cookie{Name: "origin_session", MaxAge: -1})
			return
		}
		if _, err := r.Cookie("origin_session"); err != nil {
			http.Error(w, "session missing", http.StatusUnauthorized)
		}
	}))
	t.Cleanup(origin.Close)
	upstream, _ := url.Parse(origin.URL)
	gateway := newPathTargetRegressionGateway(t)
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	for _, test := range []struct {
		path string
		want int
	}{
		{"/http://" + upstream.Host + "/account/login", http.StatusOK},
		{"/_proxy/v1/http/" + upstream.Host + "/account/protected", http.StatusOK},
		{"/http://" + upstream.Host + "/outside", http.StatusUnauthorized},
		{"/_proxy/v1/http/" + upstream.Host + "/account/logout", http.StatusOK},
		{"/http://" + upstream.Host + "/account/protected", http.StatusUnauthorized},
	} {
		response, err := client.Get(gateway.URL + test.path)
		if err != nil {
			t.Fatal(err)
		}
		drainProxyRegressionResponse(t, response)
		if response.StatusCode != test.want {
			t.Fatalf("cookie request %s status=%d, want %d", test.path, response.StatusCode, test.want)
		}
	}
}

func TestPathTargetCookieAliasesPreserveIPv6AndEscapedPaths(t *testing.T) {
	upstream, err := url.Parse("https://[fd00::10]:8920/media%20files/login?sig=%2Ftoken")
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"Set-Cookie": {"origin_session=alice; Path=/media%20files; Domain=origin.example; HttpOnly; Secure"}}
	rewritePathTargetSetCookies(header, upstream)
	want := []string{"/_proxy/v1/https/[fd00::10]:8920/media%20files", "/https://[fd00::10]:8920/media%20files"}
	values := header.Values("Set-Cookie")
	if len(values) != len(want) {
		t.Fatalf("cookie aliases=%q", values)
	}
	for index, value := range values {
		cookie, err := http.ParseSetCookie(value)
		if err != nil || cookie.Path != want[index] || cookie.Domain != "" || !cookie.HttpOnly || !cookie.Secure || strings.Contains(value, "sig=") {
			t.Fatalf("cookie alias changed origin path or leaked query: %q, err=%v", value, err)
		}
	}
}
