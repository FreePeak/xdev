package config

import (
	"reflect"
	"strings"
)

// This file guards the one CLI command that can brick the settings file.
//
// Layered loads are strict (`readSettingsFile` decodes each file with
// KnownFields(true)), so a key the `Settings` struct does not declare makes
// the *next* start reject the whole file: it is renamed to
// `*.broken-<stamp>-config.yml` and the session comes up on defaults with
// every setting lost. `xdev config set typokey 1` used to write the typo and
// report success — the damage only surfaced on the next launch, by which
// point the user's real config had been moved out of the way.

// settingsKeyOK reports whether a dotted key walks a real path in Settings.
// Inside a map field (`modelRoles.<role>`, `proxyGroups.<name>`) any segment
// is accepted: those key sets are open by design.
func settingsKeyOK(key string) bool {
	parts := strings.Split(key, ".")
	t := reflect.TypeOf(Settings{})
	for _, seg := range parts {
		if seg == "" {
			return false // empty segment: `set "a." v` writes a file nothing can load
		}
		switch deref(t).Kind() {
		case reflect.Struct:
			t = fieldByYamlName(deref(t), seg)
		case reflect.Map:
			t = deref(t).Elem()
			if t.Kind() == reflect.Interface {
				return true // free-form block (hooks.*): anything nests inside
			}
		default:
			t = nil
		}
		if t == nil {
			return false
		}
	}
	return true
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

// fieldByYamlName returns a struct field's type by its yaml key, looking
// through untagged embedded structs the way yaml.v3 does.
func fieldByYamlName(t reflect.Type, name string) reflect.Type {
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" {
			continue // unexported: invisible to yaml
		}
		tag := strings.Split(f.Tag.Get("yaml"), ",")[0]
		switch tag {
		case "-":
			continue
		case "":
			if f.Anonymous && deref(f.Type).Kind() == reflect.Struct {
				if next := fieldByYamlName(deref(f.Type), name); next != nil {
					return next
				}
				continue
			}
			if !strings.EqualFold(f.Name, name) {
				continue
			}
		default:
			if tag != name {
				continue
			}
		}
		return f.Type
	}
	return nil
}
