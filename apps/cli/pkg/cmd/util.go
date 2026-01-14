// File generated from our OpenAPI spec by Stainless. See CONTRIBUTING.md for details.

package cmd

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"os"
	"reflect"
	"strings"

	"github.com/kernel/hypeman-go/option"

	"github.com/tidwall/sjson"
	"github.com/urfave/cli/v3"
)


type fileReader struct {
	Value         io.Reader
	Base64Encoded bool
}

func (f *fileReader) Set(filename string) error {
	reader, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("failed to read file %q: %w", filename, err)
	}
	f.Value = reader
	return nil
}

func (f *fileReader) String() string {
	if f.Value == nil {
		return ""
	}
	buf := new(bytes.Buffer)
	buf.ReadFrom(f.Value)
	if f.Base64Encoded {
		return base64.StdEncoding.EncodeToString(buf.Bytes())
	}
	return buf.String()
}

func (f *fileReader) Get() any {
	return f.String()
}

func unmarshalWithReaders(data []byte, v any) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}

	rv := reflect.ValueOf(v).Elem()
	rt := rv.Type()

	for i := 0; i < rv.NumField(); i++ {
		fv := rv.Field(i)
		ft := rt.Field(i)

		jsonKey := ft.Tag.Get("json")
		if jsonKey == "" {
			jsonKey = ft.Name
		} else if idx := strings.Index(jsonKey, ","); idx != -1 {
			jsonKey = jsonKey[:idx]
		}

		rawVal, ok := fields[jsonKey]
		if !ok {
			continue
		}

		if ft.Type == reflect.TypeOf((*io.Reader)(nil)).Elem() {
			var s string
			if err := json.Unmarshal(rawVal, &s); err != nil {
				return fmt.Errorf("field %s: %w", ft.Name, err)
			}
			fv.Set(reflect.ValueOf(strings.NewReader(s)))
		} else {
			ptr := fv.Addr().Interface()
			if err := json.Unmarshal(rawVal, ptr); err != nil {
				return fmt.Errorf("field %s: %w", ft.Name, err)
			}
		}
	}

	return nil
}

func unmarshalStdinWithFlags(cmd *cli.Command, flags map[string]string, target any) error {
	var data []byte
	if isInputPiped() {
		var err error
		if data, err = io.ReadAll(os.Stdin); err != nil {
			return err
		}
	}

	// Merge CLI flags into the body
	for flag, path := range flags {
		if cmd.IsSet(flag) {
			var err error
			data, err = sjson.SetBytes(data, path, cmd.Value(flag))
			if err != nil {
				return err
			}
		}
	}

	if data != nil {
		if err := unmarshalWithReaders(data, target); err != nil {
			return fmt.Errorf("failed to unmarshal JSON: %w", err)
		}
	}

	return nil
}

func debugMiddleware(debug bool) option.Middleware {
	return func(r *http.Request, mn option.MiddlewareNext) (*http.Response, error) {
		if debug {
			logger := log.Default()

			if reqBytes, err := httputil.DumpRequest(r, true); err == nil {
				logger.Printf("Request Content:\n%s\n", reqBytes)
			}

			resp, err := mn(r)
			if err != nil {
				return resp, err
			}

			if respBytes, err := httputil.DumpResponse(resp, true); err == nil {
				logger.Printf("Response Content:\n%s\n", respBytes)
			}

			return resp, err
		}

		return mn(r)
	}
}
