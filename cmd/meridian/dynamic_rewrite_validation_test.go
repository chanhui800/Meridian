package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDynamicRewriteSessionValidatesAuthorityOnceAndRefreshesNextResponse(t *testing.T) {
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("relative=%t", relative), func(t *testing.T) {
			issuer := newStructuredDiscoveryTestIssuer(t)
			var lookups atomic.Int64
			issuer.state.runtime.resolver = dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
				lookups.Add(1)
				return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
			})
			base := mustStructuredURL(t, "https://api.example.com/live/master.m3u8")
			if relative {
				base = mustStructuredURL(t, "https://cdn.example.com/live/master.m3u8")
			}
			var manifest strings.Builder
			manifest.WriteString("#EXTM3U\n#EXT-X-TARGETDURATION:4\n")
			for segment := range 40 {
				prefix := "https://cdn.example.com/live/"
				if relative {
					prefix = ""
				}
				fmt.Fprintf(&manifest, "#EXTINF:4,\n%ssegment-%d.ts\n", prefix, segment)
			}
			manifest.WriteString("#EXT-X-ENDLIST\n")
			for response := int64(1); response <= 2; response++ {
				session := &dynamicRewriteSession{ctx: context.Background(), issuer: issuer, base: base, source: dynamicDiscoverySourceHLS, rewriteRelative: relative}
				payload, err := rewriteHLSResponse([]byte(manifest.String()), session)
				if err != nil {
					session.rollback()
					t.Fatal(err)
				}
				if !session.commit() || len(structuredCapabilityRoutes(string(payload))) != 40 {
					t.Fatal("manifest did not publish all forty segment routes")
				}
				if got := lookups.Load(); got != response {
					t.Fatalf("DNS calls after response %d = %d, want one new validation per response", response, got)
				}
			}
		})
	}
}

func TestDynamicRewriteSessionRechecksLocalInterfacesAndPolicyGeneration(t *testing.T) {
	for _, changedPolicy := range []bool{false, true} {
		t.Run(fmt.Sprintf("policy_generation=%t", changedPolicy), func(t *testing.T) {
			issuer := newStructuredDiscoveryTestIssuer(t)
			var lookups atomic.Int64
			issuer.state.runtime.resolver = dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
				lookups.Add(1)
				return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
			})
			policy := issuer.state.runtime.selfTargets.Load()
			local := false
			policy.interfaceAddrs = func() ([]net.Addr, error) {
				if local {
					return []net.Addr{&net.IPAddr{IP: net.ParseIP("1.1.1.1")}}, nil
				}
				return nil, nil
			}
			session := &dynamicRewriteSession{ctx: context.Background(), issuer: issuer, base: mustStructuredURL(t, "https://api.example.com/live/master.m3u8"), source: dynamicDiscoverySourceHLS}
			defer session.rollback()
			if _, err := session.rewrite("https://cdn.example.com/first.ts"); err != nil {
				t.Fatal(err)
			}
			if changedPolicy {
				issuer.state.runtime.selfTargets.Store(newTestDynamicSelfTargetPolicy(t))
			} else {
				local = true
			}
			_, err := session.rewrite("https://cdn.example.com/second.ts")
			if changedPolicy {
				if err != nil || lookups.Load() != 2 {
					t.Fatalf("new policy did not force fresh DNS: calls=%d err=%v", lookups.Load(), err)
				}
			} else if err == nil || lookups.Load() != 1 {
				t.Fatalf("changed local interface was not denied on cached validation: calls=%d err=%v", lookups.Load(), err)
			}
		})
	}
}

func TestDASHRewriteSessionSharesAuthorityValidationAcrossTemplates(t *testing.T) {
	issuer := newStructuredDiscoveryTestIssuer(t)
	var lookups atomic.Int64
	issuer.state.runtime.resolver = dynamicIPResolverFunc(func(context.Context, string) ([]net.IPAddr, error) {
		lookups.Add(1)
		return []net.IPAddr{{IP: net.ParseIP("1.1.1.1")}}, nil
	})
	session := &dynamicRewriteSession{ctx: context.Background(), issuer: issuer, base: mustStructuredURL(t, "https://api.example.com/live/master.mpd"), source: dynamicDiscoverySourceDASH}
	defer session.rollback()
	manifest := []byte(`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011"><Period><AdaptationSet><Representation id="v" bandwidth="100"><SegmentTemplate media="https://cdn.example.com/segment-$Number$.m4s" initialization="https://cdn.example.com/init.m4s"/></Representation></AdaptationSet></Period></MPD>`)
	if _, err := rewriteDASHResponse(manifest, session); err != nil {
		t.Fatal(err)
	}
	if lookups.Load() != 1 || !session.commit() {
		t.Fatalf("DASH templates did not share one authority validation: lookups=%d", lookups.Load())
	}
}
