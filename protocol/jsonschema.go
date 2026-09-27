package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Ограниченный валидатор JSON-схем: тип, обязательные поля, свойства,
// элементы массива, enum. Этого достаточно для списков замечаний и
// подзадач; полная реализация draft-2020 здесь не нужна.

var schemaTypes = map[string]bool{"object": true, "array": true, "string": true, "number": true, "integer": true, "boolean": true}

// CheckSchema проверяет, что сама схема из поддерживаемого подмножества.
func CheckSchema(s map[string]any) error {
	return checkSchema(s, "")
}

func checkSchema(s map[string]any, path string) error {
	at := func(f string) string {
		if path == "" {
			return f
		}
		return path + "." + f
	}
	t, _ := s["type"].(string)
	if !schemaTypes[t] {
		return fmt.Errorf("%s: неизвестный тип %q", at("type"), t)
	}
	if props, ok := s["properties"]; ok {
		pm, ok := props.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: ожидается объект", at("properties"))
		}
		for name, sub := range pm {
			sm, ok := sub.(map[string]any)
			if !ok {
				return fmt.Errorf("%s: ожидается схема", at("properties."+name))
			}
			if err := checkSchema(sm, at("properties."+name)); err != nil {
				return err
			}
		}
	}
	if items, ok := s["items"]; ok {
		im, ok := items.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: ожидается схема", at("items"))
		}
		if err := checkSchema(im, at("items")); err != nil {
			return err
		}
	}
	if req, ok := s["required"]; ok {
		if _, ok := req.([]any); !ok {
			return fmt.Errorf("%s: ожидается список имён", at("required"))
		}
	}
	return nil
}

// ValidateJSON проверяет данные по схеме; ошибка называет путь.
func ValidateJSON(raw []byte, s map[string]any) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return fmt.Errorf("не JSON: %w", err)
	}
	return validateValue(v, s, "$")
}

func validateValue(v any, s map[string]any, path string) error {
	t, _ := s["type"].(string)
	switch t {
	case "object":
		obj, ok := v.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: ожидается объект", path)
		}
		if req, ok := s["required"].([]any); ok {
			for _, r := range req {
				name, _ := r.(string)
				if _, ok := obj[name]; !ok {
					return fmt.Errorf("%s: нет обязательного поля %s", path, name)
				}
			}
		}
		if props, ok := s["properties"].(map[string]any); ok {
			for name, sub := range props {
				val, ok := obj[name]
				if !ok {
					continue
				}
				if err := validateValue(val, sub.(map[string]any), path+"."+name); err != nil {
					return err
				}
			}
		}
	case "array":
		arr, ok := v.([]any)
		if !ok {
			return fmt.Errorf("%s: ожидается массив", path)
		}
		if items, ok := s["items"].(map[string]any); ok {
			for i, el := range arr {
				if err := validateValue(el, items, fmt.Sprintf("%s[%d]", path, i)); err != nil {
					return err
				}
			}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return fmt.Errorf("%s: ожидается строка", path)
		}
		if en, ok := s["enum"].([]any); ok {
			found := false
			var names []string
			for _, e := range en {
				es, _ := e.(string)
				names = append(names, es)
				if es == str {
					found = true
				}
			}
			if !found {
				return fmt.Errorf("%s: %q не из %s", path, str, strings.Join(names, " | "))
			}
		}
	case "number", "integer":
		f, ok := v.(float64)
		if !ok {
			return fmt.Errorf("%s: ожидается число", path)
		}
		if t == "integer" && f != float64(int64(f)) {
			return fmt.Errorf("%s: ожидается целое", path)
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return fmt.Errorf("%s: ожидается true или false", path)
		}
	}
	return nil
}
