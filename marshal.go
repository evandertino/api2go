package api2go

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

type marshalingContext struct {
	root           map[string]interface{}
	rootName       string
	isSingleStruct bool
}

func makeContext(rootName string, isSingleStruct bool) *marshalingContext {
	ctx := &marshalingContext{}
	ctx.rootName = rootName
	ctx.root = map[string]interface{}{}
	ctx.root[rootName] = []interface{}{}
	ctx.isSingleStruct = isSingleStruct
	return ctx
}

//marshalError marshals all error types
func marshalError(err error) string {
	httpErr, ok := err.(HTTPError)
	if ok {
		return marshalHTTPError(httpErr)
	}

	httpErr = NewHTTPError(err, err.Error(), 500)

	return marshalHTTPError(httpErr)
}

//marshalHTTPError marshals an internal httpError
func marshalHTTPError(input HTTPError) string {
	if len(input.Errors) == 0 {
		input.Errors = []Error{Error{Title: input.msg, Status: strconv.Itoa(input.status)}}
	}

	data, err := json.Marshal(input)

	if err != nil {
		log.Println(err)
		return "{}"
	}

	return string(data)
}

// Marshal takes a struct (or slice of structs) and marshals them to a json encodable interface{} value
func Marshal(data interface{}) (interface{}, error) {
	if data == nil || data == "" {
		return nil, errors.New("marshal only works with objects")
	}

	var ctx *marshalingContext

	if reflect.TypeOf(data).Kind() == reflect.Slice {
		// We were passed a slice
		// Using Elem() here to get the slice's element type
		rootName := pluralize(jsonify(reflect.TypeOf(data).Elem().Name()))

		// Error on empty string, i.e. passed []interface{}
		if rootName == "" {
			return nil, errors.New("you passed a slice of interfaces []interface{}{...} to Marshal. we cannot determine key names from that. Use []YourObjectName{...} instead")
		}
		ctx = makeContext(rootName, false)

		// Marshal all elements
		// We iterate using reflections to save copying the slice to a []interface{}
		sliceValue := reflect.ValueOf(data)
		for i := 0; i < sliceValue.Len(); i++ {
			if err := ctx.marshalRootStruct(sliceValue.Index(i)); err != nil {
				return nil, err
			}
		}
	} else {
		// We were passed a single object
		rootName := pluralize(jsonify(reflect.TypeOf(data).Name()))
		ctx = makeContext(rootName, true)

		// Marshal the value
		if err := ctx.marshalRootStruct(reflect.ValueOf(data)); err != nil {
			return nil, err
		}
	}

	return ctx.root, nil
}

// marshalRootStruct is a more convenient name for marshalling structs at root level
func (ctx *marshalingContext) marshalRootStruct(val reflect.Value) error {
	return ctx.marshalStruct(&val, false)
}

// marshalLinkedStruct is a more convenient name for marshalling structs that were linked to a root level struct
func (ctx *marshalingContext) marshalLinkedStruct(val reflect.Value) error {
	return ctx.marshalStruct(&val, true)
}

// marshalStruct marshals a struct and places it in the context's root
func (ctx *marshalingContext) marshalStruct(val *reflect.Value, isLinked bool) error {
	result := map[string]interface{}{}
	linksMap := map[string]interface{}{}
	idFieldRegex := regexp.MustCompile("^.*ID$")

	valType := val.Type()
	for i := 0; i < val.NumField(); i++ {
		tag := valType.Field(i).Tag.Get("json")
		if tag == "-" {
			continue
		}

		field := val.Field(i)
		keyName := jsonify(valType.Field(i).Name)

		if field.Kind() == reflect.Slice {
			// A slice indicates nested objects.

			// First, check whether this is a slice of structs which we need to nest
			if field.Type().Elem().Kind() == reflect.Struct {
				ids := []interface{}{}
				for i := 0; i < field.Len(); i++ {
					id, err := idFromObject(field.Index(i))
					if err != nil {
						return err
					}
					ids = append(ids, id)

					if err := ctx.marshalLinkedStruct(field.Index(i)); err != nil {
						return err
					}
				}
				linksMap[keyName] = ids
			} else if strings.HasSuffix(keyName, "IDs") {
				// Treat slices of non-struct type as lists of IDs if the suffix is IDs
				keyName = strings.TrimSuffix(keyName, "IDs")
				linksMapReflect := reflect.TypeOf(linksMap[keyName])
				// Don't overwrite any existing links, since they came from nested structs
				if linksMap[keyName] == nil || linksMapReflect.Kind() == reflect.Slice && len(linksMap[keyName].([]interface{})) == 0 {
					ids := []interface{}{}
					for i := 0; i < field.Len(); i++ {
						id, err := idFromValue(field.Index(i))
						if err != nil {
							return err
						}
						ids = append(ids, id)
					}
					linksMap[keyName] = ids
				}
			} else {
				result[keyName] = field.Interface()

			}
		} else if keyName == "id" {
			// ID needs to be converted to string
			id, err := idFromValue(field)
			if err != nil {
				return err
			}
			result[keyName] = id
		} else if field.Type().Kind() == reflect.Ptr {
			if !field.IsNil() {
				id, err := idFromObject(field)
				if err == nil {
					linksMap[keyName] = id
					if err := ctx.marshalLinkedStruct(field.Elem()); err != nil {
						return err
					}
				} else {
					// the field is not a referenced struct, it is a normal property, so add it to the result
					result[keyName] = field.Interface()
				}
			}
		} else if idFieldRegex.MatchString(keyName) {
			keyNameWithoutID := strings.TrimSuffix(keyName, "ID")
			structFieldName := dejsonify(keyNameWithoutID)
			// struct must be preferred, only use this field if struct ptr is nil
			structFieldValue := val.FieldByName(structFieldName)
			if !structFieldValue.IsValid() {
				return fmt.Errorf("expected struct to have field %s", structFieldName)
			}
			if structFieldValue.Kind() == reflect.Ptr && structFieldValue.IsNil() {
				id, err := idFromValue(field)
				if err != nil {
					return err
				}
				if id != "" {
					linksMap[keyNameWithoutID] = id
				} else {
					linksMap[keyNameWithoutID] = nil
				}
			}
		} else {
			result[keyName] = field.Interface()
		}
	}

	if len(linksMap) > 0 {
		result["links"] = linksMap
	}

	ctx.addValue(pluralize(jsonify(valType.Name())), result, isLinked)
	return nil
}

// addValue adds an object to the context's root
// `name` should be the pluralized and underscorized object type.
func (ctx *marshalingContext) addValue(name string, val map[string]interface{}, isLinked bool) {
	if !isLinked {
		if ctx.isSingleStruct {
			ctx.root[name] = val
		} else {
			// Root objects are placed directly into the root doc
			ctx.root[name] = append(ctx.root[name].([]interface{}), val)
		}
	} else {
		// Linked objects are placed in a map under the `linked` key
		var linkedMap map[string][]interface{}
		if ctx.root["linked"] == nil {
			linkedMap = map[string][]interface{}{}
			ctx.root["linked"] = linkedMap
		} else {
			linkedMap = ctx.root["linked"].(map[string][]interface{})
		}
		if s := linkedMap[name]; s != nil {
			// check if already in linked list
			alreadyLinked := false
			for _, linked := range s {
				m := reflect.ValueOf(linked).Interface().(map[string]interface{})
				if val["id"] == m["id"] {
					alreadyLinked = true
				}
			}
			if !alreadyLinked {
				linkedMap[name] = append(s, val)
			}
		} else {
			linkedMap[name] = []interface{}{val}
		}
	}
}

// MarshalToJSON takes a struct and marshals it to JSONAPI compliant JSON
func MarshalToJSON(val interface{}) ([]byte, error) {
	result, err := Marshal(val)
	if err != nil {
		return nil, err
	}
	return json.Marshal(result)
}
