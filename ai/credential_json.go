package ai

// Ports packages/ai/src/auth/types.ts

import (
	"bytes"
	"maps"
	"reflect"
	"strings"

	"github.com/MichaelKinsy/PiG/extensions/sdk/json"
)

// Credential properties are case-sensitive JSON keys. Filter by exact tags before the Go struct decoder can fold opaque provider metadata into a known field. Extra is still collected from the original object.
// An OAuth credential (always, for tokenOnly OAuthCredentials) decodes its required tokens strictly but treats every other key as provider-owned JSON: API-key properties are not projected, and convenience strings project only string values. Extra retains the other shapes.
func unmarshalCredentialFields(data []byte, target any, tokenOnly bool) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return err
	}
	if tokenOnly || credentialObjectType(object) == CredentialOAuth {
		for _, key := range []string{"key", "apiKey", "env"} {
			delete(object, key)
		}
		for _, key := range []string{"projectId", "accountId", "enterpriseUrl", "scope"} {
			if raw, ok := object[key]; ok {
				var value string
				if json.Unmarshal(raw, &value) != nil {
					delete(object, key)
				}
			}
		}
		if tokenOnly {
			delete(object, "type")
		}
	}
	typ := reflect.TypeOf(target).Elem()
	fields := make(map[string]json.RawMessage, typ.NumField())
	for i := range typ.NumField() {
		name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		if value, present := object[name]; present {
			fields[name] = value
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return json.Unmarshal(encoded, target)
}

func credentialObjectType(object map[string]json.RawMessage) CredentialType {
	var kind CredentialType
	if raw, ok := object["type"]; ok {
		_ = json.Unmarshal(raw, &kind)
	}
	return kind
}

// Credential extensions belong to the provider. Retain unknown fields and their UTF-16 string identity through typed projection, OAuth conversion, and persistence.
// A property absent from the omitempty projection (empty, null, or a provider-defined shape) stays in Extra so its presence survives a rewrite. Required OAuth fields always serialize from their typed values.
func credentialExtra(data []byte, known []byte, oauth bool) (map[string]json.RawMessage, error) {
	var all, fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(known, &fields); err != nil {
		return nil, err
	}
	for key := range fields {
		delete(all, key)
	}
	if oauth {
		for _, key := range []string{"access", "refresh", "expires"} {
			delete(all, key)
		}
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}
func (c Credential) MarshalJSON() ([]byte, error) {
	type plain Credential
	data, err := json.Marshal(plain(c))
	if err != nil || (len(c.Extra) == 0 && c.Type != CredentialOAuth) {
		return data, err
	}
	fields := maps.Clone(c.Extra)
	if fields == nil {
		fields = map[string]json.RawMessage{}
	}
	var known map[string]json.RawMessage
	if err := json.Unmarshal(data, &known); err != nil {
		return nil, err
	}
	if c.Type == CredentialOAuth {
		// Pi's OAuth credential shape requires these fields even when empty or expired.
		known["access"], _ = json.Marshal(c.Access)
		known["refresh"], _ = json.Marshal(c.Refresh)
		known["expires"], _ = json.Marshal(c.Expires)
	}
	maps.Copy(fields, known)
	return json.Marshal(fields)
}
func (c *Credential) UnmarshalJSON(data []byte) error {
	type plain Credential
	var decoded plain
	if err := unmarshalCredentialFields(data, &decoded, false); err != nil {
		return err
	}
	known, err := json.Marshal(decoded)
	if err != nil {
		return err
	}
	decoded.Extra, err = credentialExtra(data, known, decoded.Type == CredentialOAuth)
	if err != nil {
		return err
	}
	*c = Credential(decoded)
	return nil
}
func (c *rawCredential) UnmarshalJSON(data []byte) error {
	type plain rawCredential
	var decoded plain
	if err := unmarshalCredentialFields(data, &decoded, false); err != nil {
		return err
	}
	known, err := json.Marshal(decoded)
	if err != nil {
		return err
	}
	decoded.Extra, err = credentialExtra(data, known, decoded.Type == CredentialOAuth)
	if err != nil {
		return err
	}
	// normalize migrates the legacy apiKey property; an empty legacy value is not provider metadata.
	delete(decoded.Extra, "apiKey")
	if len(decoded.Extra) == 0 {
		decoded.Extra = nil
	}
	*c = rawCredential(decoded)
	return nil
}
func (c OAuthCredentials) MarshalJSON() ([]byte, error) {
	type plain OAuthCredentials
	data, err := json.Marshal(plain(c))
	if err != nil || len(c.Extra) == 0 {
		return data, err
	}
	fields := maps.Clone(c.Extra)
	var known map[string]json.RawMessage
	if err := json.Unmarshal(data, &known); err != nil {
		return nil, err
	}
	maps.Copy(fields, known)
	return json.Marshal(fields)
}
func (c *OAuthCredentials) UnmarshalJSON(data []byte) error {
	type plain OAuthCredentials
	var decoded plain
	if err := unmarshalCredentialFields(data, &decoded, true); err != nil {
		return err
	}
	known, err := json.Marshal(decoded)
	if err != nil {
		return err
	}
	decoded.Extra, err = credentialExtra(data, known, true)
	// The credential store owns the discriminator; OAuthCredentials never carries it.
	delete(decoded.Extra, "type")
	if len(decoded.Extra) == 0 {
		decoded.Extra = nil
	}
	if err != nil {
		return err
	}
	*c = OAuthCredentials(decoded)
	return nil
}

// credentialFromOAuth preserves provider-owned fields while promoting fields understood by the credential store into its typed representation.
func credentialFromOAuth(value OAuthCredentials) (Credential, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return Credential{}, err
	}
	// Decode as an OAuth credential so provider-owned fields that share an API-key or convenience name keep their JSON shape.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return Credential{}, err
	}
	fields["type"] = json.RawMessage(`"oauth"`)
	if data, err = json.Marshal(fields); err != nil {
		return Credential{}, err
	}
	var credential Credential
	if err := json.Unmarshal(data, &credential); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

func cloneCredentialExtra(extra map[string]json.RawMessage) map[string]json.RawMessage {
	if extra == nil {
		return nil
	}
	out := make(map[string]json.RawMessage, len(extra))
	for key, value := range extra {
		out[key] = bytes.Clone(value)
	}
	return out
}
