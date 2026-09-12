package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

type dashSegmentAddressing struct {
	kind string
	node *dashXMLNode
}

func isDASHSegmentAddressing(name string) bool {
	return name == "SegmentTemplate" || name == "SegmentList" || name == "SegmentBase"
}

func dashSegmentAddressingChildRank(content dashXMLContent) int {
	if content.node == nil {
		return -1
	}
	if content.node.start.Name.Space != "urn:mpeg:dash:schema:mpd:2011" {
		return 5
	}
	switch content.node.start.Name.Local {
	case "Initialization":
		return 0
	case "RepresentationIndex":
		return 1
	case "SegmentTimeline":
		return 2
	case "BitstreamSwitching":
		return 3
	case "SegmentURL":
		return 4
	default:
		return 5
	}
}

func mergeDASHSegmentAddressing(inherited, local *dashSegmentAddressing) *dashSegmentAddressing {
	if local == nil {
		if inherited == nil {
			return nil
		}
		return &dashSegmentAddressing{kind: inherited.kind, node: cloneDASHXMLNode(inherited.node)}
	}
	if inherited == nil || inherited.kind != local.kind {
		return &dashSegmentAddressing{kind: local.kind, node: cloneDASHXMLNode(local.node)}
	}
	merged := cloneDASHXMLNode(inherited.node)
	for _, attribute := range local.node.start.Attr {
		replaced := false
		for index := range merged.start.Attr {
			if merged.start.Attr[index].Name == attribute.Name {
				merged.start.Attr[index] = attribute
				replaced = true
				break
			}
		}
		if !replaced {
			merged.start.Attr = append(merged.start.Attr, attribute)
		}
	}
	localElementNames := make(map[xml.Name]bool)
	for _, child := range local.node.content {
		if child.node != nil {
			localElementNames[child.node.start.Name] = true
		}
	}
	if len(localElementNames) != 0 {
		filtered := merged.content[:0]
		for _, child := range merged.content {
			if child.node != nil && localElementNames[child.node.start.Name] {
				continue
			}
			filtered = append(filtered, child)
		}
		merged.content = filtered
		for _, child := range local.node.content {
			if child.node != nil {
				merged.content = append(merged.content, dashXMLContent{node: cloneDASHXMLNode(child.node)})
			}
		}
	}
	sort.SliceStable(merged.content, func(left, right int) bool {
		return dashSegmentAddressingChildRank(merged.content[left]) < dashSegmentAddressingChildRank(merged.content[right])
	})
	return &dashSegmentAddressing{kind: local.kind, node: merged}
}

func rewriteDASHURLAttribute(node *dashXMLNode, name string, base *url.URL, template bool, session *dynamicRewriteSession, bindings *dashTemplateBindings) error {
	index, err := dashAttributeIndex(node, name)
	if err != nil || index < 0 {
		return err
	}
	value := node.start.Attr[index].Value
	if template {
		if bindings == nil {
			return fmt.Errorf("DASH template bindings are unavailable")
		}
		rewritten, err := rewriteDASHTemplate(value, base, session, *bindings)
		if err != nil {
			return err
		}
		node.start.Attr[index].Value = rewritten
		return nil
	}
	rewritten, _, err := rewriteDASHReference(value, base, session, dynamicCapabilityKindResource)
	if err != nil {
		return err
	}
	node.start.Attr[index].Value = rewritten
	return nil
}

func validateDASHLeafElement(node *dashXMLNode, ctx context.Context) error {
	if err := validateDASHNodeNamespace(node); err != nil {
		return err
	}
	for _, child := range node.content {
		if child.node != nil {
			return fmt.Errorf("DASH segment addressing element has unsupported content")
		}
		if strings.TrimSpace(string(child.text)) != "" {
			return fmt.Errorf("DASH segment addressing element has unsupported content")
		}
	}
	return nil
}

func validateDASHSegmentTimeline(node *dashXMLNode, ctx context.Context) error {
	if err := validateDASHNodeNamespace(node); err != nil {
		return err
	}
	for _, child := range node.content {
		if child.node == nil {
			if strings.TrimSpace(string(child.text)) != "" {
				return fmt.Errorf("DASH SegmentTimeline has unsupported text")
			}
			continue
		}
		if child.node.start.Name.Space != "urn:mpeg:dash:schema:mpd:2011" {
			return fmt.Errorf("DASH SegmentTimeline has an unsupported child")
		}
		if child.node.start.Name.Local != "S" {
			return fmt.Errorf("DASH SegmentTimeline has an unsupported child")
		}
		if err := validateDASHLeafElement(child.node, ctx); err != nil {
			return err
		}
	}
	return nil
}

func rewriteDASHSegmentAddressing(addressing *dashSegmentAddressing, base *url.URL, session *dynamicRewriteSession, bindings dashTemplateBindings) error {
	if addressing == nil || addressing.node == nil || !isDASHSegmentAddressing(addressing.kind) || addressing.node.start.Name.Local != addressing.kind {
		return fmt.Errorf("DASH segment addressing is unavailable")
	}
	if err := validateDASHNodeNamespace(addressing.node); err != nil {
		return err
	}
	if addressing.kind == "SegmentTemplate" {
		for _, attribute := range []string{"media", "initialization", "index", "bitstreamSwitching"} {
			if err := rewriteDASHURLAttribute(addressing.node, attribute, base, true, session, &bindings); err != nil {
				return err
			}
		}
	}
	for _, content := range addressing.node.content {
		if content.node == nil {
			if strings.TrimSpace(string(content.text)) != "" {
				return fmt.Errorf("DASH segment addressing has unsupported text")
			}
			continue
		}
		child := content.node
		if err := validateDASHNodeNamespace(child); err != nil {
			return err
		}
		if child.start.Name.Space != "urn:mpeg:dash:schema:mpd:2011" {
			return fmt.Errorf("DASH segment addressing has an unsupported child")
		}
		switch child.start.Name.Local {
		case "SegmentURL":
			if addressing.kind != "SegmentList" {
				return fmt.Errorf("DASH SegmentURL is outside SegmentList")
			}
			for _, attribute := range []string{"media", "index"} {
				if err := rewriteDASHURLAttribute(child, attribute, base, false, session, nil); err != nil {
					return err
				}
			}
			if err := validateDASHLeafElement(child, session.ctx); err != nil {
				return err
			}
		case "Initialization", "RepresentationIndex", "BitstreamSwitching":
			if addressing.kind == "SegmentBase" && child.start.Name.Local == "BitstreamSwitching" {
				return fmt.Errorf("DASH SegmentBase has an unsupported child")
			}
			if err := rewriteDASHURLAttribute(child, "sourceURL", base, false, session, nil); err != nil {
				return err
			}
			if err := validateDASHLeafElement(child, session.ctx); err != nil {
				return err
			}
		case "SegmentTimeline":
			if addressing.kind == "SegmentBase" {
				return fmt.Errorf("DASH SegmentBase has an unsupported timeline")
			}
			if err := validateDASHSegmentTimeline(child, session.ctx); err != nil {
				return err
			}
		default:
			return fmt.Errorf("DASH segment addressing has an unsupported child")
		}
	}
	return nil
}

// validateDASHNodeNamespace accepts only the ISO/IEC 23009-1 namespace and its
// explicitly allowed attributes. The permissive "extreme" profile that used to
// tolerate foreign wrappers, xlink fetches, xml:base references, and vendor
// network attributes was removed, so every deviation is now an unsupported
// manifest feature.
func validateDASHNodeNamespace(node *dashXMLNode) error {
	if node == nil || node.start.Name.Space != "urn:mpeg:dash:schema:mpd:2011" {
		return fmt.Errorf("DASH foreign-namespace elements are unsupported")
	}
	for _, attribute := range node.start.Attr {
		if attribute.Name.Space == "http://www.w3.org/1999/xlink" && attribute.Name.Local == "href" {
			return fmt.Errorf("DASH xlink fetches are unsupported")
		}
		if attribute.Name.Space != "" && !(attribute.Name.Space == "http://www.w3.org/XML/1998/namespace" && attribute.Name.Local == "lang") {
			return fmt.Errorf("DASH foreign-namespace attributes are unsupported")
		}
	}
	return nil
}

func dashRepresentationBindings(node *dashXMLNode) (dashTemplateBindings, error) {
	var bindings dashTemplateBindings
	idIndex, err := dashAttributeIndex(node, "id")
	if err != nil {
		return bindings, err
	}
	if idIndex >= 0 {
		bindings.representationID = node.start.Attr[idIndex].Value
	}
	bandwidthIndex, err := dashAttributeIndex(node, "bandwidth")
	if err != nil {
		return bindings, err
	}
	if bandwidthIndex >= 0 {
		bindings.bandwidth = node.start.Attr[bandwidthIndex].Value
	}
	return bindings, nil
}
