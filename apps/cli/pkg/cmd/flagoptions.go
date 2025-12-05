package cmd

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"os"

	"github.com/onkernel/hypeman-cli/internal/apiform"
	"github.com/onkernel/hypeman-cli/internal/apiquery"
	"github.com/onkernel/hypeman-cli/internal/requestflag"
	"github.com/onkernel/hypeman-go/option"

	"github.com/urfave/cli/v3"
)

type BodyContentType int

const (
	MultipartFormEncoded BodyContentType = iota
	ApplicationJSON
)

func flagOptions(
	cmd *cli.Command,
	nestedFormat apiquery.NestedQueryFormat,
	arrayFormat apiquery.ArrayQueryFormat,
	bodyType BodyContentType,
) ([]option.RequestOption, error) {
	var options []option.RequestOption
	if cmd.Bool("debug") {
		options = append(options, debugMiddlewareOption)
	}

	queries := make(map[string]any)
	headers := make(map[string]any)
	body := make(map[string]any)
	if isInputPiped() {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &body); err != nil {
			return nil, err
		}
	}

	for _, flag := range cmd.Flags {
		if !flag.IsSet() {
			continue
		}
		value := flag.Get()
		if toSend, ok := value.(requestflag.RequestValue); ok {
			config := toSend.RequestConfig()
			if config.BodyPath != "" {
				body[config.BodyPath] = toSend.RequestValue()
			} else if config.QueryPath != "" {
				queries[config.QueryPath] = toSend.RequestValue()
			} else if config.HeaderPath != "" {
				headers[config.HeaderPath] = toSend.RequestValue()
			}
		} else if toSend, ok := value.([]requestflag.RequestValue); ok {
			config := toSend[0].RequestConfig()
			if config.BodyPath != "" {
				body[config.BodyPath] = requestflag.CollectRequestValues(toSend)
			} else if config.QueryPath != "" {
				queries[config.QueryPath] = requestflag.CollectRequestValues(toSend)
			} else if config.HeaderPath != "" {
				headers[config.HeaderPath] = requestflag.CollectRequestValues(toSend)
			}
		}
	}

	querySettings := apiquery.QuerySettings{
		NestedFormat: nestedFormat,
		ArrayFormat:  arrayFormat,
	}

	// Add query parameters:
	if values, err := apiquery.MarshalWithSettings(queries, querySettings); err != nil {
		return nil, err
	} else {
		for k, vs := range values {
			if len(vs) == 0 {
				options = append(options, option.WithQueryDel(k))
			} else {
				options = append(options, option.WithQuery(k, vs[0]))
				for _, v := range vs[1:] {
					options = append(options, option.WithQueryAdd(k, v))
				}
			}
		}
	}

	// Add header parameters
	if values, err := apiquery.MarshalWithSettings(headers, querySettings); err != nil {
		return nil, err
	} else {
		for k, vs := range values {
			if len(vs) == 0 {
				options = append(options, option.WithHeaderDel(k))
			} else {
				options = append(options, option.WithHeader(k, vs[0]))
				for _, v := range vs[1:] {
					options = append(options, option.WithHeaderAdd(k, v))
				}
			}
		}
	}

	switch bodyType {
	case MultipartFormEncoded:
		buf := new(bytes.Buffer)
		writer := multipart.NewWriter(buf)
		if err := apiform.MarshalWithSettings(body, writer, apiform.FormatComma); err != nil {
			return nil, err
		}
		if err := writer.Close(); err != nil {
			return nil, err
		}
		options = append(options, option.WithRequestBody(writer.FormDataContentType(), buf))
	case ApplicationJSON:
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		options = append(options, option.WithRequestBody("application/json", bodyBytes))
	default:
		panic("Invalid body content type!")
	}

	return options, nil
}
