package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/mimecast/dtail/internal/color"
)

// The JSON schema shipped in examples/ must describe exactly the keys the
// lenient config decoder understands. These tests keep the two in sync so a new
// config field cannot silently be rejected by schema validation again.

const schemaPath = "../../examples/dtail.schema.json"

type schemaNode = map[string]any

func loadSchema(t *testing.T) schemaNode {
	t.Helper()
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	var schema schemaNode
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	return schema
}

// lookupRef returns the definition a local "#/definitions/<name>" reference
// points to, or an error for any other or dangling reference.
func lookupRef(root schemaNode, ref string) (schemaNode, error) {
	name, found := strings.CutPrefix(ref, "#/definitions/")
	if !found {
		return nil, fmt.Errorf("unsupported $ref %q", ref)
	}
	defs, _ := root["definitions"].(schemaNode)
	target, ok := defs[name].(schemaNode)
	if !ok {
		return nil, fmt.Errorf("dangling $ref %q", ref)
	}
	return target, nil
}

// resolve follows a local "#/definitions/<name>" reference.
func resolve(t *testing.T, root, node schemaNode) schemaNode {
	t.Helper()
	ref, ok := node["$ref"].(string)
	if !ok {
		return node
	}
	target, err := lookupRef(root, ref)
	if err != nil {
		t.Fatal(err)
	}
	return resolve(t, root, target)
}

// supportedKeywords lists every keyword the test validator below implements,
// plus the annotations it may ignore. Any other keyword would be silently
// skipped by validate, so TestSchemaUsesOnlySupportedKeywords rejects it.
var supportedKeywords = map[string]bool{
	"$schema": true, "$ref": true, "definitions": true,
	"type": true, "enum": true, "pattern": true, "minimum": true, "maximum": true,
	"properties": true, "additionalProperties": true,
	"items": true, "additionalItems": true, "minItems": true,
	"description": true, "default": true, "deprecated": true,
}

// annotationKeywords may sit next to "$ref". Assertions may not: draft-07
// validators ignore every sibling of "$ref", 2019-09 validators apply them.
var annotationKeywords = map[string]bool{"description": true, "default": true, "deprecated": true}

var supportedTypes = map[string]bool{
	"object": true, "array": true, "string": true, "boolean": true, "integer": true,
}

// checkKeywords walks every subschema and reports keywords validate does not
// implement, unresolvable references, assertions next to "$ref", type arrays
// and patterns Go cannot compile.
func checkKeywords(root, node schemaNode, path string) []string {
	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, path+": "+fmt.Sprintf(format, args...))
	}
	for _, key := range sortedKeys(node) {
		if !supportedKeywords[key] {
			fail("keyword %q is not supported by the test validator", key)
		}
		if key == "definitions" && path != "$" {
			fail("definitions are only supported at the root")
		}
	}
	if ref, ok := node["$ref"]; ok {
		refString, isString := ref.(string)
		if !isString {
			fail("$ref must be a string")
		} else if _, err := lookupRef(root, refString); err != nil {
			fail("%v", err)
		}
		for _, key := range sortedKeys(node) {
			if key != "$ref" && !annotationKeywords[key] {
				fail("keyword %q next to $ref", key)
			}
		}
	}
	if typ, ok := node["type"]; ok {
		if name, isString := typ.(string); !isString || !supportedTypes[name] {
			fail("unsupported type %v", typ)
		}
	}
	if pattern, ok := node["pattern"]; ok {
		if patternString, isString := pattern.(string); !isString {
			fail("pattern must be a string")
		} else if _, err := regexp.Compile(patternString); err != nil {
			fail("pattern does not compile: %v", err)
		}
	}
	subschemas := func(key string, value any) {
		child, ok := value.(schemaNode)
		if !ok {
			fail("%s must be a schema object", key)
			return
		}
		errs = append(errs, checkKeywords(root, child, path+"/"+key)...)
	}
	for _, key := range []string{"definitions", "properties"} {
		if children, ok := node[key]; ok {
			childMap, isMap := children.(schemaNode)
			if !isMap {
				fail("%s must be an object", key)
				continue
			}
			for _, name := range sortedKeys(childMap) {
				subschemas(key+"/"+name, childMap[name])
			}
		}
	}
	if extra, ok := node["additionalProperties"]; ok {
		if _, isBool := extra.(bool); !isBool {
			subschemas("additionalProperties", extra)
		}
	}
	if extra, ok := node["additionalItems"]; ok {
		if _, isBool := extra.(bool); !isBool {
			fail("additionalItems must be a boolean")
		}
	}
	switch items := node["items"].(type) {
	case nil:
	case []any:
		for i, item := range items {
			subschemas(fmt.Sprintf("items/%d", i), item)
		}
	default:
		subschemas("items", items)
	}
	return errs
}

func TestSchemaUsesOnlySupportedKeywords(t *testing.T) {
	schema := loadSchema(t)
	if errs := checkKeywords(schema, schema, "$"); len(errs) > 0 {
		t.Fatalf("schema uses constructs the test validator does not check:\n%s",
			strings.Join(errs, "\n"))
	}
}

// TestCheckKeywordsRejectsUnsupportedConstructs makes sure the keyword walk
// itself is not vacuous.
func TestCheckKeywordsRejectsUnsupportedConstructs(t *testing.T) {
	tests := []struct {
		name    string
		schema  string
		wantErr string
	}{
		{name: "unknown keyword", schema: `{"properties": {"A": {"type": "string", "maxLength": 3}}}`,
			wantErr: `keyword "maxLength"`},
		{name: "combinator", schema: `{"anyOf": [{"type": "string"}]}`, wantErr: `keyword "anyOf"`},
		{name: "type array", schema: `{"type": ["string", "null"]}`, wantErr: "unsupported type"},
		{name: "dangling ref in definitions",
			schema:  `{"definitions": {"a": {"type": "object", "additionalProperties": {"$ref": "#/definitions/b"}}}}`,
			wantErr: `dangling $ref "#/definitions/b"`},
		{name: "foreign ref", schema: `{"items": {"$ref": "other.json#/x"}}`, wantErr: "unsupported $ref"},
		{name: "assertion next to ref",
			schema:  `{"definitions": {"a": {"type": "string"}}, "properties": {"A": {"$ref": "#/definitions/a", "minimum": 1}}}`,
			wantErr: `keyword "minimum" next to $ref`},
		{name: "bad pattern", schema: `{"pattern": "("}`, wantErr: "pattern does not compile"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var schema schemaNode
			if err := json.Unmarshal([]byte(tt.schema), &schema); err != nil {
				t.Fatalf("parse schema: %v", err)
			}
			errs := strings.Join(checkKeywords(schema, schema, "$"), "\n")
			if !strings.Contains(errs, tt.wantErr) {
				t.Fatalf("errors %q do not contain %q", errs, tt.wantErr)
			}
		})
	}
}

// jsonFields returns the JSON keys encoding/json decodes into typ, including the
// promoted fields of embedded structs, keyed to their struct fields.
func jsonFields(typ reflect.Type) map[string]reflect.StructField {
	fields := make(map[string]reflect.StructField)
	for _, field := range reflect.VisibleFields(typ) {
		tag := field.Tag.Get("json")
		if tag == "-" || !field.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if field.Anonymous && name == "" && field.Type.Kind() == reflect.Struct {
			continue // its fields are promoted and visited separately
		}
		if name == "" {
			name = field.Name
		}
		fields[name] = field
	}
	return fields
}

func schemaType(kind reflect.Kind) string {
	switch kind {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Struct, reflect.Map:
		return "object"
	default:
		return "unsupported:" + kind.String()
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// compareType checks that the schema node describes values of typ: matching
// JSON types, recursing into struct fields, map values, slice items and the
// items of fixed-size arrays.
func compareType(t *testing.T, root, node schemaNode, typ reflect.Type, path string) {
	t.Helper()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	node = resolve(t, root, node)
	want := schemaType(typ.Kind())
	if node["type"] != want {
		t.Errorf("%s: schema type = %v, want %s", path, node["type"], want)
		return
	}
	switch typ.Kind() {
	case reflect.Struct:
		compareStruct(t, root, node, typ, path)
	case reflect.Map:
		values, ok := node["additionalProperties"].(schemaNode)
		if !ok {
			t.Errorf("%s: map schema must describe its values in additionalProperties", path)
			return
		}
		compareType(t, root, values, typ.Elem(), path+"[*]")
	case reflect.Slice:
		items, ok := node["items"].(schemaNode)
		if !ok {
			t.Errorf("%s: slice schema must describe its items with one schema", path)
			return
		}
		compareType(t, root, items, typ.Elem(), path+"[]")
	case reflect.Array:
		items, ok := node["items"].([]any)
		if !ok || len(items) != typ.Len() {
			t.Errorf("%s: array schema must list %d tuple items", path, typ.Len())
			return
		}
		if node["minItems"] != float64(typ.Len()) || node["additionalItems"] != false {
			t.Errorf("%s: array schema must require exactly %d items", path, typ.Len())
		}
		for i, item := range items {
			compareType(t, root, item.(schemaNode), typ.Elem(), fmt.Sprintf("%s[%d]", path, i))
		}
	}
}

// compareStruct checks that an object schema lists exactly the JSON fields of
// typ, rejects unknown keys, and describes every field with compareType.
func compareStruct(t *testing.T, root, node schemaNode, typ reflect.Type, path string) {
	t.Helper()
	if node["additionalProperties"] != false {
		t.Errorf("%s: schema must set additionalProperties to false", path)
	}
	props, _ := node["properties"].(schemaNode)
	fields := jsonFields(typ)
	for _, name := range sortedKeys(fields) {
		if _, ok := props[name]; !ok {
			t.Errorf("%s.%s: config field is missing from %s", path, name, schemaPath)
		}
	}
	for _, name := range sortedKeys(props) {
		field, ok := fields[name]
		if !ok {
			t.Errorf("%s.%s: schema property has no matching config field", path, name)
			continue
		}
		fieldType := field.Type
		if fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if _, ok := props[name].(schemaNode)["description"]; !ok && fieldType.Kind() != reflect.Struct &&
			!strings.Contains(path, "TermColors") {
			t.Errorf("%s.%s: schema property lacks a description", path, name)
		}
		compareType(t, root, props[name].(schemaNode), fieldType, path+"."+name)
	}
}

// compareDefaults checks every "default" annotation against the value the
// built-in default configuration actually holds.
func compareDefaults(t *testing.T, root, node schemaNode, value reflect.Value, path string) {
	t.Helper()
	if value.Kind() == reflect.Pointer {
		value = value.Elem()
	}
	props, _ := resolve(t, root, node)["properties"].(schemaNode)
	fields := jsonFields(value.Type())
	for _, name := range sortedKeys(props) {
		field, ok := fields[name]
		if !ok {
			continue // reported by TestSchemaMatchesConfigStructs
		}
		prop := props[name].(schemaNode)
		fieldValue := value.FieldByIndex(field.Index)
		if want, ok := prop["default"]; ok {
			raw, err := json.Marshal(fieldValue.Interface())
			if err != nil {
				t.Fatalf("%s.%s: marshal default: %v", path, name, err)
			}
			var got any
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("%s.%s: unmarshal default: %v", path, name, err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%s.%s: schema default %v, built-in default %v", path, name, want, got)
			}
		}
		if fieldValue.Kind() == reflect.Struct ||
			(fieldValue.Kind() == reflect.Pointer && fieldValue.Elem().Kind() == reflect.Struct) {
			compareDefaults(t, root, prop, fieldValue, path+"."+name)
		}
	}
}

func TestSchemaDefaultsMatchBuiltInDefaults(t *testing.T) {
	schema := loadSchema(t)
	defaults := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	compareDefaults(t, schema, schema, reflect.ValueOf(defaults), "$")
}

func TestSchemaMatchesConfigStructs(t *testing.T) {
	schema := loadSchema(t)
	// initializer is the value the config file is decoded into, so its fields
	// (the Client, Server and Common sections) form the top level.
	compareType(t, schema, schema, reflect.TypeOf(initializer{}), "$")
}

func TestSchemaHasNoMisspelledReferences(t *testing.T) {
	raw, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	if bytes.Contains(raw, []byte(`"#ref"`)) {
		t.Fatal(`schema contains "#ref"; use "$ref" so the reference is enforced`)
	}
}

// validate is a minimal JSON-schema validator covering the keywords the dtail
// schema uses: $ref, type, enum, pattern, minimum, maximum, properties,
// additionalProperties, items (single or tuple), additionalItems and minItems.
// TestSchemaUsesOnlySupportedKeywords guarantees the schema uses nothing else,
// and only annotations sit next to "$ref".
func validate(root, node schemaNode, value any, path string) []string {
	if ref, ok := node["$ref"].(string); ok {
		target, err := lookupRef(root, ref)
		if err != nil {
			return []string{path + ": " + err.Error()}
		}
		return validate(root, target, value, path)
	}
	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, path+": "+fmt.Sprintf(format, args...))
	}
	switch node["type"] {
	case "object":
		object, ok := value.(map[string]any)
		if !ok {
			fail("want object, got %T", value)
			return errs
		}
		props, _ := node["properties"].(schemaNode)
		for _, key := range sortedKeys(object) {
			if propNode, ok := props[key].(schemaNode); ok {
				errs = append(errs, validate(root, propNode, object[key], path+"."+key)...)
				continue
			}
			switch extra := node["additionalProperties"].(type) {
			case bool:
				if !extra {
					fail("unknown key %q", key)
				}
			case schemaNode:
				errs = append(errs, validate(root, extra, object[key], path+"."+key)...)
			}
		}
	case "array":
		array, ok := value.([]any)
		if !ok {
			fail("want array, got %T", value)
			return errs
		}
		if minItems, ok := node["minItems"].(float64); ok && float64(len(array)) < minItems {
			fail("want at least %v items, got %d", minItems, len(array))
		}
		switch items := node["items"].(type) {
		case schemaNode:
			for i, item := range array {
				errs = append(errs, validate(root, items, item, fmt.Sprintf("%s[%d]", path, i))...)
			}
		case []any:
			for i, item := range array {
				if i >= len(items) {
					if node["additionalItems"] == false {
						fail("too many items")
					}
					break
				}
				errs = append(errs, validate(root, items[i].(schemaNode), item, fmt.Sprintf("%s[%d]", path, i))...)
			}
		}
	case "string":
		text, ok := value.(string)
		if !ok {
			fail("want string, got %T", value)
			return errs
		}
		if pattern, ok := node["pattern"].(string); ok && !regexp.MustCompile(pattern).MatchString(text) {
			fail("%q does not match the pattern %s", text, pattern)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			fail("want boolean, got %T", value)
		}
	case "integer":
		number, ok := value.(float64)
		if !ok || number != float64(int64(number)) {
			fail("want integer, got %v", value)
			return errs
		}
		if minimum, ok := node["minimum"].(float64); ok && number < minimum {
			fail("%v is below the minimum %v", number, minimum)
		}
		if maximum, ok := node["maximum"].(float64); ok && number > maximum {
			fail("%v is above the maximum %v", number, maximum)
		}
	}
	if enum, ok := node["enum"].([]any); ok {
		found := false
		for _, allowed := range enum {
			if allowed == value {
				found = true
				break
			}
		}
		if !found {
			fail("%v is not one of %v", value, enum)
		}
	}
	return errs
}

func validateConfigText(t *testing.T, schema schemaNode, text string) []string {
	t.Helper()
	var document any
	if err := json.Unmarshal([]byte(text), &document); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return validate(schema, schema, document, "$")
}

func TestSchemaAcceptsShippedConfigs(t *testing.T) {
	schema := loadSchema(t)
	paths := []string{"../../examples/dtail.json.example", "../../docker/dtail.json"}
	integrationConfigs, err := filepath.Glob("../../integrationtests/*.json")
	if err != nil {
		t.Fatalf("glob integration configs: %v", err)
	}
	paths = append(paths, integrationConfigs...)

	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if errs := validateConfigText(t, schema, string(raw)); len(errs) > 0 {
			t.Errorf("%s does not validate:\n%s", path, strings.Join(errs, "\n"))
		}
		// The shipped configs must also consist only of keys dtail decodes.
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&initializer{}); err != nil {
			t.Errorf("%s contains a key dtail does not decode: %v", path, err)
		}
	}
}

// TestSchemaAcceptsEveryDefaultField marshals the complete default
// configuration, which sets every field, and validates it against the schema.
func TestSchemaAcceptsEveryDefaultField(t *testing.T) {
	schema := loadSchema(t)
	defaults := initializer{
		Common: newDefaultCommonConfig(),
		Server: newDefaultServerConfig(),
		Client: newDefaultClientConfig(),
	}
	defaults.Server.HostKeyFile = "cache/ssh_host_key"
	defaults.Server.HostKeyPath = "cache/ssh_host_key"
	defaults.Server.AuthorizedKeysPath = "/etc/dserver/authorized_keys"
	defaults.Server.KeyExchanges = []string{"curve25519-sha256"}
	defaults.Server.Ciphers = []string{"aes256-gcm@openssh.com"}
	defaults.Server.MACs = []string{"hmac-sha2-256-etm@openssh.com"}
	defaults.Server.Permissions.Users = map[string][]string{"alice": {"readfiles:^/var/log/.*"}}
	common := jobCommons{Name: "job", Enable: true, Files: "/var/log/*.log",
		Query: "from STATS select count($line) group by $hostname", Outfile: "out.$today.csv",
		Discovery: "file", Servers: []string{"server1"}, AllowFrom: []string{"server1"}}
	defaults.Server.Schedule = []Scheduled{{jobCommons: common, TimeRange: [2]int{0, 24}}}
	defaults.Server.Continuous = []Continuous{{jobCommons: common, RestartOnDayChange: true}}
	defaults.Common.HostnameOverride = "host"
	// Logger and LogDir default to empty (the per-command default applies);
	// the schema rejects an empty logger name, so set real values.
	defaults.Common.Logger = DefaultServerLogger
	defaults.Common.LogDir = DefaultServerLogDir
	defaults.Common.ExperimentalFeaturesEnable = true
	defaults.Client.KnownHostsPath = "known_hosts"
	defaults.Client.AuthKeyPath = "id_rsa"
	defaults.Client.AuthKeyDisable = true
	defaults.Client.LogPayload = true

	raw, err := json.Marshal(defaults)
	if err != nil {
		t.Fatalf("marshal defaults: %v", err)
	}
	// The in-memory palette holds escape sequences, not the names a config
	// file uses, so validate the palette separately through the example file.
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("unmarshal defaults: %v", err)
	}
	delete(document["Client"].(map[string]any), "TermColors")
	if errs := validate(schema, schema, document, "$"); len(errs) > 0 {
		t.Fatalf("default configuration does not validate:\n%s", strings.Join(errs, "\n"))
	}
}

func TestSchemaValidation(t *testing.T) {
	schema := loadSchema(t)
	tests := []struct {
		name    string
		config  string
		wantErr string
	}{
		{name: "idle session timeout", config: `{"Server": {"IdleSessionTimeoutS": 900}}`},
		{name: "idle session timeout default sentinel", config: `{"Server": {"IdleSessionTimeoutS": 0}}`},
		{name: "lower-case logger names", config: `{"Common": {"Logger": "fout", "LogLevel": "info", "LogRotation": "daily"}}`},
		{name: "removed turbo key", config: `{"Server": {"TurboBoostDisable": true}}`, wantErr: `unknown key "TurboBoostDisable"`},
		{name: "unknown top-level section", config: `{"Servers": {}}`, wantErr: `unknown key "Servers"`},
		{name: "ssh port range", config: `{"Common": {"SSHPort": 70000}}`, wantErr: "above the maximum"},
		{name: "mixed-case logger names", config: `{"Common": {"Logger": "FOut", "LogLevel": "WaRn", "LogRotation": "Signal"}}`},
		{name: "empty log level", config: `{"Common": {"LogLevel": ""}}`},
		{name: "logger name", config: `{"Common": {"Logger": "syslog"}}`, wantErr: "does not match the pattern"},
		{name: "logger name prefix", config: `{"Common": {"Logger": "stdoutx"}}`, wantErr: "does not match the pattern"},
		{name: "empty logger", config: `{"Common": {"Logger": ""}}`, wantErr: "does not match the pattern"},
		{name: "log level name", config: `{"Common": {"LogLevel": "warning"}}`, wantErr: "does not match the pattern"},
		{name: "log rotation name", config: `{"Common": {"LogRotation": "weekly"}}`, wantErr: "does not match the pattern"},
		{name: "frame size limit", config: `{"Server": {"MaxCommandFrameSize": 1}}`},
		{name: "disabled frame size guard", config: `{"Server": {"MaxCommandFrameSize": 0}}`, wantErr: "below the minimum"},
		{name: "per-user permissions", config: `{"Server": {"Permissions": {"Users": {"alice": ["readfiles:^/var/log/"]}}}}`},
		{name: "per-user permission type", config: `{"Server": {"Permissions": {"Users": {"alice": [1]}}}}`, wantErr: "want string"},
		{name: "color name", config: `{"Client": {"TermColors": {"Server": {"TextFg": "Purple"}}}}`, wantErr: "does not match the pattern"},
		{name: "mixed-case color names", config: `{"Client": {"TermColors": {"Server": {"TextFg": "rEd", "TextBg": "BLACK", "TextAttr": "underline"}}}}`},
		{name: "empty attribute", config: `{"Client": {"TermColors": {"Server": {"TextAttr": ""}}}}`},
		{name: "empty color", config: `{"Client": {"TermColors": {"Server": {"TextFg": "", "TextBg": ""}}}}`},
		{name: "deprecated prefixed color", config: `{"Client": {"TermColors": {"Server": {"TextBg": "BgCyan"}}}}`, wantErr: "does not match the pattern"},
		{name: "deprecated prefixed attribute", config: `{"Client": {"TermColors": {"Server": {"TextAttr": "AttrDim"}}}}`, wantErr: "does not match the pattern"},
		{name: "attribute name", config: `{"Client": {"TermColors": {"Server": {"TextAttr": "Strike"}}}}`, wantErr: "does not match the pattern"},
		{name: "deprecated raw escape color", config: `{"Client": {"TermColors": {"Server": {"TextFg": "\u001b[37m"}}}}`, wantErr: "does not match the pattern"},
		{name: "time range length", config: `{"Server": {"Schedule": [{"TimeRange": [1]}]}}`, wantErr: "at least 2 items"},
		{name: "wrong type", config: `{"Server": {"IdleSessionTimeoutS": "900"}}`, wantErr: "want integer"},
		{name: "ssh port lowest", config: `{"Common": {"SSHPort": 1}}`},
		{name: "ssh port highest", config: `{"Common": {"SSHPort": 65535}}`},
		{name: "ssh port zero", config: `{"Common": {"SSHPort": 0}}`, wantErr: "below the minimum"},
		{name: "ssh port above range", config: `{"Common": {"SSHPort": 65536}}`, wantErr: "above the maximum"},
		{name: "one concurrent cat", config: `{"Server": {"MaxConcurrentCats": 1}}`},
		{name: "zero concurrent cats", config: `{"Server": {"MaxConcurrentCats": 0}}`, wantErr: "below the minimum"},
		{name: "one concurrent tail", config: `{"Server": {"MaxConcurrentTails": 1}}`},
		{name: "zero concurrent tails", config: `{"Server": {"MaxConcurrentTails": 0}}`, wantErr: "below the minimum"},
		{name: "one connection", config: `{"Server": {"MaxConnections": 1}}`},
		{name: "zero connections", config: `{"Server": {"MaxConnections": 0}}`, wantErr: "below the minimum"},
		{name: "negative connections", config: `{"Server": {"MaxConnections": -1}}`, wantErr: "below the minimum"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := strings.Join(validateConfigText(t, schema, tt.config), "\n")
			if tt.wantErr == "" && errs != "" {
				t.Fatalf("unexpected validation errors:\n%s", errs)
			}
			if tt.wantErr != "" && !strings.Contains(errs, tt.wantErr) {
				t.Fatalf("validation errors %q do not contain %q", errs, tt.wantErr)
			}
		})
	}
}

// parseGoFile parses one runtime source file for TestSchemaAcceptsRuntimeLogNames.
func parseGoFile(t *testing.T, path string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return file
}

// caseStrings returns the string literals of every case clause of the switch
// statements in function funcName.
func caseStrings(t *testing.T, file *ast.File, funcName string) []string {
	t.Helper()
	var names []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != funcName {
			continue
		}
		ast.Inspect(fn, func(node ast.Node) bool {
			clause, ok := node.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, expr := range clause.List {
				if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					name, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("unquote %s: %v", lit.Value, err)
					}
					names = append(names, name)
				}
			}
			return true
		})
	}
	if len(names) == 0 {
		t.Fatalf("no case strings found in %s", funcName)
	}
	return names
}

// mapLiteralKeys returns the string keys of the map literal assigned to the
// package variable varName.
func mapLiteralKeys(t *testing.T, file *ast.File, varName string) []string {
	t.Helper()
	var names []string
	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || spec.Names[0].Name != varName || len(spec.Values) != 1 {
			return true
		}
		literal, ok := spec.Values[0].(*ast.CompositeLit)
		if !ok {
			t.Fatalf("%s is not a composite literal", varName)
		}
		for _, element := range literal.Elts {
			kv, ok := element.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if lit, ok := kv.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				name, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				names = append(names, name)
			}
		}
		return false
	})
	if len(names) == 0 {
		t.Fatalf("no keys found in %s", varName)
	}
	return names
}

// constNamesOfType returns the names of the constants declared with type
// typeName, with suffix removed and lower-cased (DailyRotation -> daily).
func constNamesOfType(t *testing.T, file *ast.File, typeName, suffix string) []string {
	t.Helper()
	var names []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		// Within a const block a spec without type and values repeats the
		// previous spec's type (the implicit iota style), so track it.
		var blockType string
		for _, spec := range gen.Specs {
			value := spec.(*ast.ValueSpec)
			switch {
			case value.Type != nil:
				blockType = ""
				if ident, ok := value.Type.(*ast.Ident); ok {
					blockType = ident.Name
				}
			case len(value.Values) > 0:
				blockType = ""
			}
			if blockType != typeName {
				continue
			}
			for _, name := range value.Names {
				names = append(names, strings.ToLower(strings.TrimSuffix(name.Name, suffix)))
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("no constants of type %s found", typeName)
	}
	return names
}

// caseVariants returns name in lower, upper and alternating mixed case.
func caseVariants(name string) []string {
	mixed := []rune(strings.ToLower(name))
	for i := range mixed {
		if i%2 == 0 {
			mixed[i] = unicode.ToUpper(mixed[i])
		}
	}
	return []string{strings.ToLower(name), strings.ToUpper(name), string(mixed)}
}

// TestSchemaAcceptsRuntimeLogNames derives every log level, logger and log
// rotation name from the runtime sources, so a name added to the runtime (or
// dropped from a schema pattern) fails here instead of drifting silently.
func TestSchemaAcceptsRuntimeLogNames(t *testing.T) {
	schema := loadSchema(t)
	levelFile := parseGoFile(t, "../io/dlog/level.go")
	factoryFile := parseGoFile(t, "../io/dlog/loggers/factory.go")
	strategyFile := parseGoFile(t, "../io/dlog/loggers/strategy.go")

	// The level constants and the parser's switch must agree, otherwise the
	// switch is no longer a complete source of level names.
	levels := caseStrings(t, levelFile, "newLevel")
	constLevels := constNamesOfType(t, levelFile, "level", "")
	for _, name := range constLevels {
		if !containsString(levels, name) {
			t.Fatalf("level constant %q has no case in newLevel %v", name, levels)
		}
	}
	loggerNames := mapLiteralKeys(t, factoryFile, "loggerRegistry")
	rotations := constNamesOfType(t, strategyFile, "Rotation", "Rotation")
	// NewStrategy must still parse the daily name explicitly.
	for _, name := range caseStrings(t, strategyFile, "NewStrategy") {
		if !containsString(rotations, name) {
			t.Fatalf("NewStrategy case %q has no Rotation constant %v", name, rotations)
		}
	}

	groups := []struct {
		key   string
		names []string
	}{
		{key: "LogLevel", names: levels},
		{key: "Logger", names: loggerNames},
		{key: "LogRotation", names: rotations},
	}
	for _, group := range groups {
		for _, name := range group.names {
			variants := caseVariants(name)
			if name == "" {
				variants = []string{""}
			}
			for _, variant := range variants {
				t.Run(fmt.Sprintf("%s=%q", group.key, variant), func(t *testing.T) {
					document, err := json.Marshal(map[string]any{
						"Common": map[string]any{group.key: variant},
					})
					if err != nil {
						t.Fatal(err)
					}
					if errs := validateConfigText(t, schema, string(document)); len(errs) > 0 {
						t.Fatalf("runtime name rejected:\n%s", strings.Join(errs, "\n"))
					}
				})
			}
		}
	}
}

// TestSchemaAcceptsRuntimeColorNames checks that every colour and attribute
// name the runtime decodes (in any letter case) passes the schema, and that
// the schema accepts nothing the decoder would reject.
func TestSchemaAcceptsRuntimeColorNames(t *testing.T) {
	schema := loadSchema(t)
	colorFile := parseGoFile(t, "../color/color.go")
	groups := []struct {
		key   string
		names []string
	}{
		{key: "TextFg", names: caseStrings(t, colorFile, "ToFgColor")},
		{key: "TextBg", names: caseStrings(t, colorFile, "ToBgColor")},
		{key: "TextAttr", names: caseStrings(t, colorFile, "ToAttribute")},
	}
	for _, group := range groups {
		for _, name := range group.names {
			variants := caseVariants(name)
			if name == "" {
				variants = []string{""}
			}
			for _, variant := range variants {
				t.Run(fmt.Sprintf("%s=%q", group.key, variant), func(t *testing.T) {
					document, err := json.Marshal(map[string]any{"Client": map[string]any{
						"TermColors": map[string]any{"Server": map[string]any{group.key: variant}},
					}})
					if err != nil {
						t.Fatal(err)
					}
					if errs := validateConfigText(t, schema, string(document)); len(errs) > 0 {
						t.Fatalf("runtime name rejected by the schema:\n%s", strings.Join(errs, "\n"))
					}
					// A string never fails to decode now; a name the decoder does
					// not know falls back to the default with a warning instead.
					color.TakeConfigWarnings()
					var in initializer
					if err := json.Unmarshal(document, &in); err != nil {
						t.Fatalf("schema-valid name rejected by the decoder: %v", err)
					}
					if warnings := color.TakeConfigWarnings(); len(warnings) > 0 {
						t.Fatalf("schema-valid name fell back to the default:\n%s",
							strings.Join(warnings, "\n"))
					}
				})
			}
		}
	}
}
