/*
MIT License

Copyright (c) 2025 Pavel Konovalov

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package configuration

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"unsafe"

	"gopkg.in/yaml.v3"
)

var (
	EnvVarPrefix = ""
	ReadFromFile = ReadConfigFromYMLFile
)

// ReadFromEnv reads environment variables into struct fields based on `env` and `yaml` tags
func ReadFromEnv(s any) error {
	return readFromEnv(reflect.ValueOf(s).Elem(), "")
}

// ListEnv lists all environment variables that can be used to override configuration values
func ListEnv(s any) []string {
	return listEnv(reflect.ValueOf(s).Elem(), "")
}

// readFromEnv reads environment variables into struct fields based on `env` and `yaml` tags
func readFromEnv(v reflect.Value, parentTag string) error {
	var err error

	for i := 0; i < v.Type().NumField() && err == nil; i++ {
		field := v.Type().Field(i)
		val := v.Field(i)

		switch field.Type.Kind() {
		case reflect.Struct:
			err = readFromEnv(val, field.Tag.Get("yaml"))
		default:
			if field.Tag.Get("env") == "true" {
				ptg := strings.Split(parentTag, ",")
				tg := strings.Split(field.Tag.Get("yaml"), ",")

				envVar := ""
				if ptg[0] != "" {
					envVar = strings.ToUpper(ptg[0] + "_" + tg[0])
				} else {
					envVar = strings.ToUpper(tg[0])
				}

				envFullName := fmt.Sprintf("%s%s", EnvVarPrefix, envVar)
				envVal := os.Getenv(envFullName)

				if envVal != "" {
					switch val.Kind() {
					case reflect.String:
						val.SetString(envVal)
					case reflect.Bool:
						if strings.ToUpper(envVal) == "TRUE" || envVar == "1" || strings.ToUpper(envVal) == "Y" {
							val.SetBool(true)
						} else {
							val.SetBool(false)
						}
					case reflect.Int, reflect.Int64:
						var intVar int
						if intVar, err = strconv.Atoi(envVal); err == nil {
							val.SetInt(int64(intVar))
						} else {
							err = fmt.Errorf("%s %v", envVar, err)
						}
					case reflect.Int32:
						var intVar int
						if intVar, err = strconv.Atoi(envVal); err == nil {
							val.SetInt(int64(intVar))
						} else {
							err = fmt.Errorf("%s %v", envVar, err)
						}
					case reflect.Slice:
						if val.Type().String() == "[]string" {
							for item := range strings.SplitSeq(envVal, ",") {
								if item != "" {
									val.Set(reflect.Append(val, reflect.ValueOf(item)))
								}
							}
						} else {
							err = fmt.Errorf("%s: unsupported slice type '%s'", envVar, val.Type().String())
						}
					default:
						err = fmt.Errorf("%s: unsupported type '%s'", envVar, val.Kind().String())
					}
				}
			}
		}
	}
	return err
}

// listEnv lists all environment variables that can be used to override configuration values
func listEnv(v reflect.Value, parentTag string) []string {

	var envVarList []string

	for i := 0; i < v.Type().NumField(); i++ {
		field := v.Type().Field(i)
		val := v.Field(i)

		switch field.Type.Kind() {
		case reflect.Struct:
			envVarList = append(envVarList, listEnv(val, field.Tag.Get("yaml"))...)
		default:
			if field.Tag.Get("env") == "true" {
				ptg := strings.Split(parentTag, ",")
				tg := strings.Split(field.Tag.Get("yaml"), ",")

				if ptg[0] != "" {
					envVarList = append(envVarList, strings.ToUpper(EnvVarPrefix+ptg[0]+"_"+tg[0]))
				} else {
					envVarList = append(envVarList, strings.ToUpper(EnvVarPrefix+tg[0]))
				}
			}
		}
	}
	return envVarList
}

// ReadConfigFromYMLFile reads configuration from a YAML file into the provided struct
func ReadConfigFromYMLFile(configurationFile string, config any) error {
	var f *os.File
	var err error

	if f, err = os.Open(configurationFile); err != nil {
		return err
	}

	defer f.Close()

	decoder := yaml.NewDecoder(f)
	if err = decoder.Decode(config); err != nil {
		return err
	}
	return nil
}

// WriteConfigToYMLFile writes the provided struct configuration to a YAML file
func WriteConfigToYMLFile(configurationFile string, config any) error {
	var f *os.File
	var err error

	if f, err = os.Create(configurationFile); err != nil {
		return err
	}

	defer f.Close()

	encoder := yaml.NewEncoder(f)
	if err = encoder.Encode(config); err != nil {
		return err
	}
	return nil

}

// Read from yml configuration file and env variables
func Read(configurationFile string, s any) error {

	if err := ReadFromFile(configurationFile, s); err != nil {
		return err
	}

	return ReadFromEnv(s)
}

// TODO implement setting default values to configuration
// setField sets value to a structs field by field's value
// field is field of struct
// value is value to set to the struct
func setField(field reflect.Value, value any) {
	t := field.Type()
	p := unsafe.Pointer(field.UnsafeAddr())
	v := reflect.ValueOf(value)
	reflect.NewAt(t, p).Elem().Set(v)
}

// Update updates pStructure by default_ values where key is field name of pStructure, value of map is value of
// structure by concrete field. structure and pStructure are the struct and pointer to struct respectively
func Update(structure any, pStructure any, default_ map[string]any) {
	// TODO: change pointer to interface and interface to single object
	t := reflect.TypeOf(structure)
	for field := range t.Fields() {
		fName := field.Name
		defaultValue, ok := default_[fName]
		if ok {
			value := reflect.ValueOf(pStructure).Elem()
			field := value.FieldByName(fName)
			setField(field, defaultValue)
		}
	}
}
