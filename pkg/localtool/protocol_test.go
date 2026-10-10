package localtool

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestServeSingleRequestResponseAndBusinessError(t *testing.T) {
	req := Request{ProtocolVersion: 1, Name: "read_skill", Arguments: []byte(`{"name":"review"}`), Config: []byte(`{"skills_dir":"skills"}`)}
	data, _ := json.Marshal(req)
	var out bytes.Buffer
	err := Serve(bytes.NewReader(data), &out, func(got Request) (Response, error) {
		if got.Name != req.Name || string(got.Config) != string(req.Config) {
			t.Fatal(got)
		}
		return Response{Text: "中文", Error: "business error"}, nil
	})
	var result Response
	if err != nil || Decode(out.Bytes(), &result) != nil || result.ProtocolVersion != 1 || result.Text != "中文" || result.Error != "business error" {
		t.Fatal(err, out.String())
	}
	for _, input := range []string{`null`, `{}`, `{"protocol_version":2,"name":"t","arguments":{},"config":{}}`, `{"protocol_version":1,"name":"t","arguments":null,"config":{}}`, string(data) + " {}", `{"protocol_version":1,"name":"t","arguments":{},"config":{},"unexpected":true}`} {
		out.Reset()
		called := false
		err := Serve(strings.NewReader(input), &out, func(Request) (Response, error) { called = true; return Response{}, nil })
		if err == nil || called || out.Len() != 0 {
			t.Fatal("invalid request reached tool", input, err)
		}
	}
	out.Reset()
	if err := Serve(bytes.NewReader(data), &out, func(Request) (Response, error) { return Response{}, errors.New("execution failed") }); err == nil || out.Len() != 0 {
		t.Fatal("handler failure written as success", err)
	}
}
