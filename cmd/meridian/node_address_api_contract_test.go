package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func nodeAddressAPIRequest(t *testing.T, app *App, method, path string, payload map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s %s payload: %v", method, path, err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	if method == http.MethodPost && path == "/api/nodes" {
		app.handleNodes(response, request)
	} else {
		app.handleNodeByID(response, request)
	}
	return response
}

func nodeAddressAPICreatePayload(name, mode string) map[string]any {
	return map[string]any{
		"name": name, "address": "", "address_v4": "", "address_v6": "",
		"dns_publish": mode, "listen_interface": "eth-test", "port": 19443,
		"billing_mode": "outbound", "reset_day": 1,
		"controller_url": "https://panel.example.test:9090",
	}
}

func decodeNodeAddressAPICreateResponse(t *testing.T, response *httptest.ResponseRecorder) ControlNode {
	t.Helper()
	var body struct {
		Node ControlNode `json:"node"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode node create response %q: %v", response.Body.String(), err)
	}
	return body.Node
}

func decodeNodeAddressAPIUpdateResponse(t *testing.T, response *httptest.ResponseRecorder) ControlNode {
	t.Helper()
	var node ControlNode
	if err := json.Unmarshal(response.Body.Bytes(), &node); err != nil {
		t.Fatalf("decode node update response %q: %v", response.Body.String(), err)
	}
	return node
}

func nodeAddressAPIField(node ControlNode, field string) string {
	switch field {
	case "address_v4":
		return node.AddressV4
	case "address_v6":
		return node.AddressV6
	default:
		return ""
	}
}

func createNodeThroughAddressAPI(t *testing.T, app *App, name, mode string) ControlNode {
	t.Helper()
	response := nodeAddressAPIRequest(t, app, http.MethodPost, "/api/nodes", nodeAddressAPICreatePayload(name, mode))
	if response.Code != http.StatusOK {
		t.Fatalf("POST /api/nodes status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"listen_interface"`) {
		t.Fatalf("POST response still exposes deprecated listen_interface: %s", response.Body.String())
	}
	return decodeNodeAddressAPICreateResponse(t, response)
}

func updateNodeThroughAddressAPI(t *testing.T, app *App, node ControlNode, payload map[string]any) (*httptest.ResponseRecorder, ControlNode) {
	t.Helper()
	if _, ok := payload["name"]; !ok {
		payload["name"] = node.Name
	}
	if _, ok := payload["port"]; !ok {
		payload["port"] = node.Port
	}
	if _, ok := payload["dns_publish"]; !ok {
		payload["dns_publish"] = node.DNSPublish
	}
	if _, ok := payload["billing_mode"]; !ok {
		payload["billing_mode"] = node.BillingMode
	}
	if _, ok := payload["reset_day"]; !ok {
		payload["reset_day"] = node.ResetDay
	}
	if _, ok := payload["enabled"]; !ok {
		enabled := node.Enabled
		payload["enabled"] = enabled
	}
	path := fmt.Sprintf("/api/nodes/%d", node.ID)
	response := nodeAddressAPIRequest(t, app, http.MethodPut, path, payload)
	if response.Code != http.StatusOK {
		return response, ControlNode{}
	}
	if strings.Contains(response.Body.String(), `"listen_interface"`) {
		t.Fatalf("PUT response still exposes deprecated listen_interface: %s", response.Body.String())
	}
	return response, decodeNodeAddressAPIUpdateResponse(t, response)
}

func TestNodeAddressAPIAllowsSelectedFamilyToBeSavedEmpty(t *testing.T) {
	for _, mode := range []string{"v4", "v6"} {
		t.Run(mode, func(t *testing.T) {
			app := newTestApp(t)
			node := createNodeThroughAddressAPI(t, app, "pending-"+mode, mode)
			if node.DNSPublish != mode || node.AddressV4 != "" || node.AddressV6 != "" {
				t.Fatalf("POST did not preserve pending %s mode with empty addresses: %+v", mode, node)
			}
			var storedInterface string
			if err := app.db.db.QueryRow(`SELECT listen_interface FROM control_nodes WHERE id=?`, node.ID).Scan(&storedInterface); err != nil {
				t.Fatal(err)
			}
			if storedInterface != "" {
				t.Fatalf("legacy listen_interface was persisted on POST: %q", storedInterface)
			}
			if _, err := desiredNodeDNSAddresses(node); err == nil {
				t.Fatal("DNS generation succeeded before an address for the selected family was reported")
			}

			payload := nodeAddressAPICreatePayload(node.Name, mode)
			_, updated := updateNodeThroughAddressAPI(t, app, node, payload)
			if updated.DNSPublish != mode || updated.AddressV4 != "" || updated.AddressV6 != "" {
				t.Fatalf("PUT did not preserve pending %s mode with empty addresses: %+v", mode, updated)
			}
			if err := app.db.db.QueryRow(`SELECT listen_interface FROM control_nodes WHERE id=?`, updated.ID).Scan(&storedInterface); err != nil {
				t.Fatal(err)
			}
			if storedInterface != "" {
				t.Fatalf("legacy listen_interface was persisted on PUT: %q", storedInterface)
			}
			if _, err := desiredNodeDNSAddresses(updated); err == nil {
				t.Fatal("DNS generation succeeded after PUT but before an address for the selected family was reported")
			}
		})
	}
}

func TestNodeAddressAPIKeepsOppositeFamilyAddressPendingAndResumesWhenSelectedAddressArrives(t *testing.T) {
	tests := []struct {
		mode, oppositeField, oppositeAddress, selectedField, selectedAddress, recordType string
	}{
		{"v4", "address_v6", "2001:db8::44", "address_v4", "203.0.113.44", "A"},
		{"v6", "address_v4", "203.0.113.45", "address_v6", "2001:db8::45", "AAAA"},
	}
	for _, test := range tests {
		t.Run(test.mode, func(t *testing.T) {
			app := newTestApp(t)
			payload := nodeAddressAPICreatePayload(test.mode+" waiting for selected family", test.mode)
			payload[test.oppositeField] = test.oppositeAddress
			createResponse := nodeAddressAPIRequest(t, app, http.MethodPost, "/api/nodes", payload)
			if createResponse.Code != http.StatusOK {
				t.Fatalf("POST with only the opposite-family address status=%d body=%s", createResponse.Code, createResponse.Body.String())
			}
			node := decodeNodeAddressAPICreateResponse(t, createResponse)
			if node.DNSPublish != test.mode || nodeAddressAPIField(node, test.oppositeField) != test.oppositeAddress || nodeAddressAPIField(node, test.selectedField) != "" {
				t.Fatalf("POST did not preserve the opposite-family-only input as pending: %+v", node)
			}
			if _, err := desiredNodeDNSAddresses(node); err == nil {
				t.Fatalf("%s DNS generation succeeded while only the opposite family was supplied", test.mode)
			}

			updatePayload := nodeAddressAPICreatePayload(node.Name, test.mode)
			updatePayload[test.oppositeField] = test.oppositeAddress
			updatePayload[test.selectedField] = test.selectedAddress
			response, updated := updateNodeThroughAddressAPI(t, app, node, updatePayload)
			if response.Code != http.StatusOK {
				t.Fatalf("PUT with the selected %s address status=%d body=%s", test.mode, response.Code, response.Body.String())
			}
			if updated.DNSPublish != test.mode || nodeAddressAPIField(updated, test.oppositeField) != test.oppositeAddress || nodeAddressAPIField(updated, test.selectedField) != test.selectedAddress {
				t.Fatalf("PUT did not save the selected %s address: %+v", test.mode, updated)
			}
			desired, err := desiredNodeDNSAddresses(updated)
			if err != nil {
				t.Fatalf("DNS generation did not resume after the selected %s address arrived: %v", test.mode, err)
			}
			if len(desired) != 1 || desired[0].recordType != test.recordType || desired[0].address != test.selectedAddress {
				t.Fatalf("selected %s DNS records=%+v, want one matching %s record", test.mode, desired, test.recordType)
			}
		})
	}
}

func TestNodeAddressAPIRejectsInvalidDedicatedAddressFieldsOnPostAndPut(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value string
	}{
		{name: "malformed IPv4", field: "address_v4", value: "not-an-ip"},
		{name: "IPv6 in IPv4 field", field: "address_v4", value: "2001:db8::4"},
		{name: "private IPv4", field: "address_v4", value: "192.168.1.4"},
		{name: "CGNAT IPv4", field: "address_v4", value: "100.64.0.4"},
		{name: "IPv4 in IPv6 field", field: "address_v6", value: "203.0.113.4"},
		{name: "private IPv6", field: "address_v6", value: "fd00::4"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Run("POST", func(t *testing.T) {
				app := newTestApp(t)
				payload := nodeAddressAPICreatePayload("invalid-post-"+test.field, "auto")
				payload[test.field] = test.value
				response := nodeAddressAPIRequest(t, app, http.MethodPost, "/api/nodes", payload)
				if response.Code != http.StatusBadRequest {
					t.Fatalf("POST invalid %s=%q status=%d body=%s, want 400", test.field, test.value, response.Code, response.Body.String())
				}
			})
			t.Run("PUT", func(t *testing.T) {
				app := newTestApp(t)
				node := createNodeThroughAddressAPI(t, app, "invalid-put-"+test.field, "auto")
				payload := nodeAddressAPICreatePayload(node.Name, "auto")
				payload[test.field] = test.value
				response, _ := updateNodeThroughAddressAPI(t, app, node, payload)
				if response.Code != http.StatusBadRequest {
					t.Fatalf("PUT invalid %s=%q status=%d body=%s, want 400", test.field, test.value, response.Code, response.Body.String())
				}
				stored, err := app.db.controlNodeByID(node.ID, time.Now().UTC())
				if err != nil {
					t.Fatalf("read existing node after rejected PUT: %v", err)
				}
				if stored.AddressV4 != "" || stored.AddressV6 != "" {
					t.Fatalf("rejected PUT changed stored addresses: v4=%q v6=%q", stored.AddressV4, stored.AddressV6)
				}
			})
		})
	}
}
