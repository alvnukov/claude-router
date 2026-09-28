package privacy

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestSupportedPropertyNamesRemainStructural(t *testing.T) {
	e := supportedContentEngine(t)
	raw := []byte(`{"tools":[{"name":"run","input_schema":{"type":"object","properties":{"PrivateOrg":{"type":"string","description":"PrivateOrg"}},"required":["PrivateOrg"],"propertyNames":{"enum":["PrivateOrg"]}}}]}`)
	out, req, err := e.Mask(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer req.Close()
	var got struct {
		Tools []struct {
			InputSchema struct {
				Properties    map[string]any
				Required      []string
				PropertyNames struct{ Enum []string }
			} `json:"input_schema"`
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	schema := got.Tools[0].InputSchema
	if _, exists := schema.Properties["PrivateOrg"]; !exists || len(schema.Required) != 1 || schema.Required[0] != "PrivateOrg" {
		t.Fatal("structural property names changed")
	}
	if len(schema.PropertyNames.Enum) != 1 || schema.PropertyNames.Enum[0] != schema.Required[0] {
		t.Fatalf("schema no longer allows its required property: %s", out)
	}
	if bytes.Contains(out, []byte(`"description":"PrivateOrg"`)) {
		t.Fatal("known description not masked")
	}
}
