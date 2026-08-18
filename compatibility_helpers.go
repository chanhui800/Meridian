package main

import "fmt"

const manualRedirectMaxHops = 5

// dynamicDiscoverySourcesEqual compares canonical source lists without sorting;
// profile normalization already defines their order.
func dynamicDiscoverySourcesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func dynamicSafeDomainAllowed(host string, rules []DynamicDomainRule) bool {
	normalizedHost, isIP, err := normalizeDynamicHost(host)
	if err != nil || isIP {
		return false
	}
	if len(rules) == 0 {
		return true
	}
	return dynamicDomainRuleMatches(normalizedHost, rules)
}

func dynamicSelectableDiscoverySourceSetsForProfile(profile string) ([][]string, bool) {
	full, ok := dynamicDiscoverySourcesForProfile(profile)
	if !ok {
		return nil, false
	}
	withoutPlaybackInfo := make([]string, 0, len(full))
	for _, source := range full {
		if source != dynamicDiscoverySourcePlaybackInfo {
			withoutPlaybackInfo = append(withoutPlaybackInfo, source)
		}
	}
	return [][]string{full, withoutPlaybackInfo}, true
}

func normalizeDynamicDiscoverySourcesForAPI(profile string, sources []string) ([]string, error) {
	normalized, err := normalizeDynamicDiscoverySources(sources)
	if err != nil {
		return nil, err
	}
	if err := validateDynamicDiscoverySourcesForProfile(profile, normalized); err != nil {
		return nil, err
	}
	return normalized, nil
}

func validateSelectableDynamicDiscoverySources(profile string, sources []string) error {
	selectable, ok := dynamicSelectableDiscoverySourceSetsForProfile(profile)
	if !ok {
		return fmt.Errorf("unsupported dynamic discovery profile")
	}
	for _, allowed := range selectable {
		if dynamicDiscoverySourcesEqual(sources, allowed) {
			return nil
		}
	}
	return fmt.Errorf("dynamic_discovery_sources must equal a selectable source set for profile %q", profile)
}
