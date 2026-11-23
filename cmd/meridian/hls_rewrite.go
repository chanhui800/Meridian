package main

import (
	"bytes"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

type hlsAttributeSpan struct {
	name       string
	valueStart int
	valueEnd   int
	quoted     bool
}

func isHLSAttributeNameByte(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '-'
}

func parseHLSAttributeList(value string) ([]hlsAttributeSpan, error) {
	spans := make([]hlsAttributeSpan, 0, 8)
	seen := make(map[string]bool)
	for offset := 0; offset < len(value); {
		nameStart := offset
		for offset < len(value) && isHLSAttributeNameByte(value[offset]) {
			offset++
		}
		if nameStart == offset || offset >= len(value) || value[offset] != '=' {
			return nil, fmt.Errorf("invalid HLS attribute list")
		}
		name := value[nameStart:offset]
		if seen[name] {
			return nil, fmt.Errorf("duplicate HLS attribute")
		}
		seen[name] = true
		offset++
		span := hlsAttributeSpan{name: name}
		if offset < len(value) && value[offset] == '"' {
			span.quoted = true
			offset++
			span.valueStart = offset
			for offset < len(value) && value[offset] != '"' {
				if value[offset] < 0x20 || value[offset] == 0x7f {
					return nil, fmt.Errorf("invalid HLS quoted string")
				}
				offset++
			}
			if offset >= len(value) {
				return nil, fmt.Errorf("unterminated HLS quoted string")
			}
			span.valueEnd = offset
			offset++
		} else {
			span.valueStart = offset
			for offset < len(value) && value[offset] != ',' {
				if value[offset] <= 0x20 || value[offset] == 0x7f {
					return nil, fmt.Errorf("invalid HLS attribute value")
				}
				offset++
			}
			span.valueEnd = offset
			if span.valueStart == span.valueEnd {
				return nil, fmt.Errorf("empty HLS attribute value")
			}
		}
		if len(spans) >= globalDynamicMaxHLSAttributesPerTag {
			return nil, fmt.Errorf("HLS attribute count exceeds its limit")
		}
		spans = append(spans, span)
		if offset == len(value) {
			break
		}
		if value[offset] != ',' || offset+1 == len(value) {
			return nil, fmt.Errorf("invalid HLS attribute delimiter")
		}
		offset++
	}
	if len(spans) == 0 {
		return nil, fmt.Errorf("empty HLS attribute list")
	}
	return spans, nil
}
func hlsAttributeValue(value string, attributes []hlsAttributeSpan, name string) (string, bool) {
	for _, attribute := range attributes {
		if attribute.name == name {
			return value[attribute.valueStart:attribute.valueEnd], true
		}
	}
	return "", false
}

func validateHLSURIAttributes(tag, value string, attributes []hlsAttributeSpan) error {
	uri, hasURI := hlsAttributeValue(value, attributes, "URI")
	requireURI := func() error {
		if !hasURI || uri == "" {
			return fmt.Errorf("HLS tag requires a URI attribute")
		}
		return nil
	}
	requireIdentityAES128 := func(method string) error {
		keyFormat, hasKeyFormat := hlsAttributeValue(value, attributes, "KEYFORMAT")
		if !strings.EqualFold(method, "AES-128") || hasKeyFormat && keyFormat != "identity" {
			return fmt.Errorf("HLS DRM key formats are unsupported")
		}
		return nil
	}
	switch tag {
	case "#EXT-X-KEY":
		method, hasMethod := hlsAttributeValue(value, attributes, "METHOD")
		if !hasMethod {
			return fmt.Errorf("HLS key tag requires METHOD")
		}
		if strings.EqualFold(method, "NONE") {
			if hasURI {
				return fmt.Errorf("HLS METHOD=NONE must not include URI")
			}
			return nil
		}
		if err := requireIdentityAES128(method); err != nil {
			return err
		}
		return requireURI()
	case "#EXT-X-SESSION-KEY":
		method, hasMethod := hlsAttributeValue(value, attributes, "METHOD")
		if !hasMethod || strings.EqualFold(method, "NONE") {
			return fmt.Errorf("HLS session key requires an encryption METHOD")
		}
		if err := requireIdentityAES128(method); err != nil {
			return err
		}
		return requireURI()
	case "#EXT-X-MEDIA":
		mediaType, hasType := hlsAttributeValue(value, attributes, "TYPE")
		if !hasType {
			return fmt.Errorf("HLS media tag requires TYPE")
		}
		switch strings.ToUpper(mediaType) {
		case "SUBTITLES":
			return requireURI()
		case "CLOSED-CAPTIONS":
			if hasURI {
				return fmt.Errorf("HLS closed captions must not include URI")
			}
		}
		return nil
	case "#EXT-X-SESSION-DATA":
		_, hasDataID := hlsAttributeValue(value, attributes, "DATA-ID")
		_, hasValue := hlsAttributeValue(value, attributes, "VALUE")
		if !hasDataID || hasURI == hasValue {
			return fmt.Errorf("HLS session data requires DATA-ID and exactly one of URI or VALUE")
		}
		return nil
	case "#EXT-X-PRELOAD-HINT":
		hintType, hasType := hlsAttributeValue(value, attributes, "TYPE")
		if !hasType || strings.ToUpper(hintType) != "PART" && strings.ToUpper(hintType) != "MAP" {
			return fmt.Errorf("unsupported HLS preload hint type")
		}
		return requireURI()
	case "#EXT-X-RENDITION-REPORT":
		if err := requireURI(); err != nil {
			return err
		}
		reference, err := url.Parse(uri)
		if err != nil || reference.IsAbs() || reference.Host != "" {
			return newDynamicPolicyDenialError(fmt.Errorf("HLS rendition report URI must be relative"))
		}
		return nil
	case "#EXT-X-CONTENT-STEERING":
		serverURI, hasServerURI := hlsAttributeValue(value, attributes, "SERVER-URI")
		if !hasServerURI || serverURI == "" {
			return fmt.Errorf("HLS content steering requires SERVER-URI")
		}
		return nil
	case "#EXT-X-DATERANGE":
		return nil
	default:
		return requireURI()
	}
}

func rewriteHLSURIKind(value string, session *dynamicRewriteSession, kind string) (string, error) {
	if value == "" || value != strings.TrimSpace(value) || containsDynamicUnsafeRune(value) || strings.Contains(value, `\`) {
		return "", newDynamicPolicyDenialError(fmt.Errorf("invalid HLS URI"))
	}
	if strings.Contains(value, "{$") {
		return "", newDynamicPolicyDenialError(fmt.Errorf("HLS variable substitution is unsupported"))
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", newDynamicPolicyDenialError(fmt.Errorf("invalid HLS URI"))
	}
	if parsed.Scheme != "" && !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return "", newDynamicPolicyDenialError(fmt.Errorf("unsupported HLS URI scheme"))
	}
	return session.rewriteAgainstKind(value, session.base, kind)
}

func expandHLSVariables(value string, variables map[string]string) (string, error) {
	for expansions := 0; ; expansions++ {
		start := strings.Index(value, "{$")
		if start < 0 {
			return value, nil
		}
		if expansions >= 64 {
			return "", fmt.Errorf("HLS variable expansion count exceeds its limit")
		}
		endOffset := strings.IndexByte(value[start+2:], '}')
		if endOffset < 0 {
			return "", fmt.Errorf("unterminated HLS variable reference")
		}
		end := start + 2 + endOffset
		name := value[start+2 : end]
		replacement, ok := variables[name]
		if name == "" || !ok {
			return "", fmt.Errorf("unresolved HLS variable")
		}
		placeholderLength := end + 1 - start
		if len(replacement) > globalDynamicMaxStringBytes-len(value)+placeholderLength {
			return "", fmt.Errorf("expanded HLS URI exceeds its limit")
		}
		value = value[:start] + replacement + value[end+1:]
		if int64(len(value)) > globalDynamicMaxStringBytes {
			return "", fmt.Errorf("expanded HLS URI exceeds its limit")
		}
	}
}

func validHLSVariableName(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func hlsVariableAttributeIsQuoted(attributes []hlsAttributeSpan, name string) bool {
	for _, attribute := range attributes {
		if attribute.name == name {
			return attribute.quoted
		}
	}
	return false
}

func defineHLSVariable(line string, session *dynamicRewriteSession, variables map[string]string) error {
	colon := strings.IndexByte(line, ':')
	if colon < 0 || colon+1 >= len(line) {
		return fmt.Errorf("invalid HLS variable definition")
	}
	attributes, err := parseHLSAttributeList(line[colon+1:])
	if err != nil {
		return err
	}
	if imported, ok := hlsAttributeValue(line[colon+1:], attributes, "IMPORT"); ok {
		if imported == "" || len(attributes) != 1 || !hlsVariableAttributeIsQuoted(attributes, "IMPORT") {
			return fmt.Errorf("invalid HLS imported variable definition")
		}
		return fmt.Errorf("HLS imported variables are unsupported")
	}
	if queryParam, ok := hlsAttributeValue(line[colon+1:], attributes, "QUERYPARAM"); ok {
		if !attributes[0].quoted || len(attributes) != 1 || !validHLSVariableName(queryParam) || session == nil || session.base == nil {
			return fmt.Errorf("invalid HLS query parameter variable")
		}
		values, err := url.ParseQuery(session.base.RawQuery)
		if err != nil {
			return fmt.Errorf("invalid HLS variable query string")
		}
		value, exists := values[queryParam]
		if !exists || len(value) == 0 {
			return fmt.Errorf("missing HLS query parameter variable")
		}
		if len(value[0]) > globalDynamicMaxStringBytes {
			return fmt.Errorf("HLS query parameter variable exceeds its limit")
		}
		if _, duplicate := variables[queryParam]; duplicate || len(variables) >= 64 {
			return fmt.Errorf("HLS variable count exceeds its limit")
		}
		variables[queryParam] = value[0]
		return nil
	}
	name, hasName := hlsAttributeValue(line[colon+1:], attributes, "NAME")
	value, hasValue := hlsAttributeValue(line[colon+1:], attributes, "VALUE")
	if !hasName || !hasValue || len(attributes) != 2 || !hlsVariableAttributeIsQuoted(attributes, "NAME") ||
		!hlsVariableAttributeIsQuoted(attributes, "VALUE") || !validHLSVariableName(name) {
		return fmt.Errorf("invalid HLS variable definition")
	}
	if _, exists := variables[name]; exists || len(variables) >= 64 {
		return fmt.Errorf("duplicate or excessive HLS variable definition")
	}
	if int64(len(value)) > globalDynamicMaxStringBytes {
		return fmt.Errorf("HLS variable value exceeds its limit")
	}
	value, err = expandHLSVariables(value, variables)
	if err != nil {
		return err
	}
	variables[name] = value
	return nil
}

func rewriteHLSURIKindWithVariables(value string, session *dynamicRewriteSession, kind string, variables map[string]string) (string, error) {
	expanded, err := expandHLSVariables(value, variables)
	if err != nil {
		return "", newDynamicPolicyDenialError(err)
	}
	return rewriteHLSURIKind(expanded, session, kind)
}

func hlsAttributeResourceKind(tag, name string) string {
	switch tag {
	case "#EXT-X-MEDIA", "#EXT-X-I-FRAME-STREAM-INF", "#EXT-X-IMAGE-STREAM-INF", "#EXT-X-RENDITION-REPORT", "#EXT-X-CONTENT-STEERING":
		return dynamicCapabilityKindManifest
	case "#EXT-X-DATERANGE":
		if name == "X-ASSET-URI" || name == "X-ASSET-LIST" {
			return dynamicCapabilityKindManifest
		}
	}
	return dynamicCapabilityKindResource
}

func rewriteHLSAttributeLine(line string, names map[string]bool, session *dynamicRewriteSession, variables map[string]string) (string, error) {
	colon := strings.IndexByte(line, ':')
	if colon < 0 || colon+1 >= len(line) {
		return "", fmt.Errorf("invalid HLS tag attribute list")
	}
	attributes, err := parseHLSAttributeList(line[colon+1:])
	if err != nil {
		return "", err
	}
	if err := validateHLSURIAttributes(line[:colon], line[colon+1:], attributes); err != nil {
		return "", err
	}
	type replacement struct {
		start int
		end   int
		value string
	}
	replacements := make([]replacement, 0, 2)
	for _, attribute := range attributes {
		if !names[attribute.name] {
			continue
		}
		if !attribute.quoted {
			return "", fmt.Errorf("HLS URI attribute must be quoted")
		}
		start := colon + 1 + attribute.valueStart
		end := colon + 1 + attribute.valueEnd
		rewritten, err := rewriteHLSURIKindWithVariables(line[start:end], session, hlsAttributeResourceKind(line[:colon], attribute.name), variables)
		if err != nil {
			return "", err
		}
		if rewritten != line[start:end] {
			replacements = append(replacements, replacement{start: start, end: end, value: rewritten})
		}
	}
	for index := len(replacements) - 1; index >= 0; index-- {
		replacement := replacements[index]
		line = line[:replacement.start] + replacement.value + line[replacement.end:]
	}
	return line, nil
}

func knownHLSManifestTag(tag string) bool {
	switch tag {
	case "#EXTINF",
		"#EXT-X-VERSION", "#EXT-X-DEFINE", "#EXT-X-START", "#EXT-X-INDEPENDENT-SEGMENTS",
		"#EXT-X-KEY", "#EXT-X-BYTERANGE", "#EXT-X-DISCONTINUITY", "#EXT-X-MAP",
		"#EXT-X-PROGRAM-DATE-TIME", "#EXT-X-GAP", "#EXT-X-BITRATE", "#EXT-X-DATERANGE",
		"#EXT-X-TARGETDURATION", "#EXT-X-MEDIA-SEQUENCE", "#EXT-X-DISCONTINUITY-SEQUENCE",
		"#EXT-X-ENDLIST", "#EXT-X-PLAYLIST-TYPE", "#EXT-X-I-FRAMES-ONLY", "#EXT-X-ALLOW-CACHE",
		"#EXT-X-PART", "#EXT-X-SERVER-CONTROL", "#EXT-X-PART-INF", "#EXT-X-PRELOAD-HINT",
		"#EXT-X-RENDITION-REPORT", "#EXT-X-SKIP", "#EXT-X-MEDIA", "#EXT-X-STREAM-INF",
		"#EXT-X-I-FRAME-STREAM-INF", "#EXT-X-SESSION-DATA", "#EXT-X-SESSION-KEY",
		"#EXT-X-CONTENT-STEERING", "#EXT-X-IMAGE-STREAM-INF":
		return true
	default:
		return false
	}
}

func rewriteHLSResponse(payload []byte, session *dynamicRewriteSession) ([]byte, error) {
	if session == nil || session.issuer == nil && !session.pathTargetMode || !utf8.Valid(payload) || len(payload) == 0 {
		return nil, fmt.Errorf("invalid HLS manifest")
	}
	sawCRLF := false
	sawLF := false
	for index, value := range payload {
		if (value < 0x20 && value != '\t' && value != '\r' && value != '\n') || value == 0x7f {
			return nil, fmt.Errorf("HLS manifest contains a control character")
		}
		if value == '\r' && (index+1 >= len(payload) || payload[index+1] != '\n') {
			return nil, fmt.Errorf("HLS manifest contains a bare carriage return")
		}
		if value == '\r' {
			sawCRLF = true
		}
		if value == '\n' && (index == 0 || payload[index-1] != '\r') {
			sawLF = true
		}
	}
	if sawCRLF && sawLF {
		return nil, fmt.Errorf("HLS manifest mixes newline styles")
	}
	maxLines := session.maxURLsPerResponse()*8 + 1024
	if bytes.Count(payload, []byte{'\n'})+1 > maxLines {
		return nil, fmt.Errorf("HLS manifest contains too many lines")
	}
	text := string(payload)
	separator := "\n"
	if sawCRLF {
		separator = "\r\n"
	}
	lines := strings.Split(text, separator)
	if len(lines) == 0 || lines[0] != "#EXTM3U" {
		return nil, fmt.Errorf("HLS manifest is missing EXTM3U")
	}
	uriTags := map[string]map[string]bool{
		"#EXT-X-KEY":                {"URI": true},
		"#EXT-X-MAP":                {"URI": true},
		"#EXT-X-MEDIA":              {"URI": true},
		"#EXT-X-I-FRAME-STREAM-INF": {"URI": true},
		"#EXT-X-CONTENT-STEERING":   {"SERVER-URI": true},
		"#EXT-X-SESSION-KEY":        {"URI": true},
		"#EXT-X-SESSION-DATA":       {"URI": true},
		"#EXT-X-IMAGE-STREAM-INF":   {"URI": true},
		"#EXT-X-RENDITION-REPORT":   {"URI": true},
		"#EXT-X-PRELOAD-HINT":       {"URI": true},
		"#EXT-X-PART":               {"URI": true},
		"#EXT-X-DATERANGE":          {"X-ASSET-LIST": true, "X-ASSET-URI": true, "X-URI": true},
	}
	singletons := make(map[string]bool)
	multivariant := false
	media := false
	expectVariantURI := false
	sawUnscopedURI := false
	variables := make(map[string]string)
	for index, line := range lines {
		if index&255 == 0 {
			if err := session.ctx.Err(); err != nil {
				return nil, fmt.Errorf("HLS parsing deadline exceeded")
			}
		}
		if int64(len(line)) > globalDynamicMaxStringBytes {
			return nil, fmt.Errorf("HLS line exceeds its limit")
		}
		if index == 0 {
			continue
		}
		if line == "" {
			if expectVariantURI {
				return nil, fmt.Errorf("HLS variant tag is not followed by a URI")
			}
			continue
		}
		if !strings.HasPrefix(line, "#") {
			if multivariant && !expectVariantURI {
				return nil, fmt.Errorf("HLS multivariant URI is missing STREAM-INF")
			}
			kind := dynamicCapabilityKindResource
			if expectVariantURI {
				kind = dynamicCapabilityKindManifest
				expectVariantURI = false
			} else {
				sawUnscopedURI = true
			}
			if strings.ContainsAny(line, " \t") {
				return nil, fmt.Errorf("HLS URI line contains whitespace")
			}
			if line != strings.TrimSpace(line) {
				return nil, fmt.Errorf("HLS URI line contains surrounding whitespace")
			}
			rewritten, err := rewriteHLSURIKindWithVariables(line, session, kind, variables)
			if err != nil {
				return nil, err
			}
			lines[index] = rewritten
			continue
		}
		if expectVariantURI {
			return nil, fmt.Errorf("HLS variant tag is not followed by a URI")
		}
		tag := line
		if colon := strings.IndexByte(line, ':'); colon >= 0 {
			tag = line[:colon]
		}
		// Extension tags are refused rather than rewritten. The removed
		// permissive profile used to accept unknown #EXT-X- tags with inert
		// values, but an unrecognized tag can still name a network location in
		// a form this rewriter cannot route.
		if strings.HasPrefix(tag, "#EXT") && !knownHLSManifestTag(tag) {
			return nil, fmt.Errorf("unsupported HLS tag")
		}
		switch tag {
		case "#EXT-X-DEFINE":
			if err := defineHLSVariable(line, session, variables); err != nil {
				return nil, err
			}
			// Definitions are consumed here, so unexpanded origin fragments and
			// query tokens never reach a player or a client-side URI resolver.
			lines[index] = ""
			continue
		case "#EXT-X-CONTENT-STEERING":
			if !session.pathTargetMode {
				return nil, fmt.Errorf("HLS content steering is unsupported")
			}
		case "#EXT-X-VERSION", "#EXT-X-START", "#EXT-X-INDEPENDENT-SEGMENTS", "#EXT-X-SKIP":
			if singletons[tag] {
				return nil, fmt.Errorf("duplicate singleton HLS tag")
			}
			singletons[tag] = true
		}
		if tag == "#EXT-X-VERSION" {
			versionText := strings.TrimPrefix(line, "#EXT-X-VERSION:")
			version, err := strconv.Atoi(versionText)
			if err != nil || version < 1 || version > 13 {
				return nil, fmt.Errorf("unsupported HLS protocol version")
			}
		}
		if tag == "#EXT-X-STREAM-INF" {
			expectVariantURI = true
		}
		switch tag {
		case "#EXT-X-MEDIA", "#EXT-X-STREAM-INF", "#EXT-X-I-FRAME-STREAM-INF", "#EXT-X-IMAGE-STREAM-INF", "#EXT-X-SESSION-DATA", "#EXT-X-SESSION-KEY":
			multivariant = true
			if sawUnscopedURI {
				return nil, fmt.Errorf("HLS multivariant URI is missing STREAM-INF")
			}
		case "#EXT-X-TARGETDURATION", "#EXT-X-MEDIA-SEQUENCE", "#EXT-X-DISCONTINUITY-SEQUENCE", "#EXT-X-ENDLIST", "#EXT-X-PLAYLIST-TYPE", "#EXT-X-I-FRAMES-ONLY", "#EXT-X-PART-INF", "#EXT-X-SERVER-CONTROL", "#EXT-X-PRELOAD-HINT", "#EXT-X-RENDITION-REPORT", "#EXT-X-SKIP", "#EXTINF", "#EXT-X-BYTERANGE", "#EXT-X-DISCONTINUITY", "#EXT-X-KEY", "#EXT-X-MAP", "#EXT-X-PROGRAM-DATE-TIME", "#EXT-X-DATERANGE", "#EXT-X-GAP", "#EXT-X-BITRATE", "#EXT-X-PART":
			media = true
		}
		if multivariant && media {
			return nil, fmt.Errorf("HLS manifest mixes multivariant and media tags")
		}
		if tag == "#EXT-X-DATERANGE" {
			colon := strings.IndexByte(line, ':')
			attributes, err := parseHLSAttributeList(line[colon+1:])
			if err != nil {
				return nil, err
			}
			for _, attribute := range attributes {
				if !session.pathTargetMode && (attribute.name == "X-ASSET-LIST" || attribute.name == "X-ASSET-URI") {
					return nil, fmt.Errorf("HLS interstitial asset URLs require an unavailable absolute public capability base")
				}
			}
		}
		if names := uriTags[tag]; names != nil {
			rewritten, err := rewriteHLSAttributeLine(line, names, session, variables)
			if err != nil {
				return nil, err
			}
			lines[index] = rewritten
		}
	}
	if !multivariant && !media {
		return nil, fmt.Errorf("HLS manifest type is not recognized")
	}
	if expectVariantURI {
		return nil, fmt.Errorf("HLS variant tag is not followed by a URI")
	}
	outputLimit := session.structuredOutputLimit()
	outputSize := int64(len(separator) * (len(lines) - 1))
	for _, line := range lines {
		if int64(len(line)) > outputLimit-outputSize {
			return nil, fmt.Errorf("HLS rewritten manifest exceeds its body budget")
		}
		outputSize += int64(len(line))
	}
	output := dynamicBoundedBuffer{limit: outputLimit}
	for index, line := range lines {
		if index > 0 {
			if _, err := output.WriteString(separator); err != nil {
				return nil, err
			}
		}
		if _, err := output.WriteString(line); err != nil {
			return nil, err
		}
	}
	return output.Bytes(), nil
}
