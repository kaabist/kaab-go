package kaab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

var pathParam = regexp.MustCompile(`\{[^}]+\}`)

// TestEveryOperation calls every generated method once: it must send its operation's method and
// path, and read its answer as its success response says.
func TestEveryOperation(t *testing.T) {
	if len(Operations) < 78 {
		t.Fatalf("operations = %d", len(Operations))
	}
	for _, op := range Operations {
		id, method, path, call := op[0], op[1], op[2], op[3]
		t.Run(call, func(t *testing.T) {
			f, c, _ := newFake(t)

			// Walk Client → Service[.Child] → Method.
			parts := strings.Split(call, ".")
			target := reflect.ValueOf(c).Elem()
			for _, field := range parts[:len(parts)-1] {
				target = reflect.Indirect(target).FieldByName(field)
				if !target.IsValid() {
					t.Fatalf("no service %s", field)
				}
			}
			fn := target.MethodByName(parts[len(parts)-1])
			if !fn.IsValid() {
				t.Fatalf("no method %s", call)
			}

			n := 0
			args := []reflect.Value{reflect.ValueOf(context.Background())}
			kind := fn.Type().Out(0)
			for i := 1; i < fn.Type().NumIn(); i++ {
				in := fn.Type().In(i)
				switch {
				case fn.Type().IsVariadic() && i == fn.Type().NumIn()-1:
				case in.Kind() == reflect.String:
					args = append(args, reflect.ValueOf(fmt.Sprintf("p%d", n)))
					n++
				case in.Kind() == reflect.Int:
					args = append(args, reflect.ValueOf(7))
					n++
				case in.Kind() == reflect.Pointer:
					args = append(args, reflect.Zero(in))
				default:
					body := reflect.New(in)
					_ = json.Unmarshal([]byte(`{"marker":1}`), body.Interface())
					args = append(args, body.Elem())
				}
			}

			want := path
			for i, segment := 0, pathParam.FindAllString(path, -1); i < len(segment); i++ {
				value := fmt.Sprintf("p%d", i)
				if segment[i] == "{n}" {
					value = "7"
				}
				want = strings.Replace(want, segment[i], value, 1)
			}
			answer := writeJSON(200, map[string]any{"id": "x", "next_cursor": nil})
			switch {
			case kind.Kind() == reflect.String:
				answer = func(w http.ResponseWriter) { w.Header().Set("Location", "https://s3.test/x"); w.WriteHeader(302) }
			case kind == reflect.TypeOf((*error)(nil)).Elem():
				answer = func(w http.ResponseWriter) { w.WriteHeader(204) }
			}
			f.on(method, "/v1"+want, answer)

			out := fn.Call(args)
			if errValue := out[len(out)-1]; !errValue.IsNil() {
				t.Fatalf("%s (%s): %v", call, id, errValue.Interface())
			}
			if len(f.requests) != 1 || f.requests[0].Method != method || f.requests[0].Path != "/v1"+want {
				t.Fatalf("requests = %+v, want %s /v1%s", f.requests, method, want)
			}
			if kind.Kind() == reflect.String && out[0].String() != "https://s3.test/x" {
				t.Fatalf("redirect = %q", out[0].String())
			}
		})
	}
}

func TestPushFallsBackToVersionOn409(t *testing.T) {
	f, c, _ := newFake(t)
	program := map[string]any{
		"id": "prg_1", "object": "program", "name": "demo", "description": nil, "latest_version": 2,
		"aliases": map[string]any{"latest": 2}, "canary": nil,
		"created_at": "2026-10-05T10:00:00Z", "updated_at": "2026-10-05T10:00:00Z",
	}
	version := map[string]any{
		"id": "pv_2", "object": "program_version", "program_id": "prg_1", "version": 2, "status": "published",
		"definition": map[string]any{"kaab": "1", "name": "demo", "steps": []any{}}, "definition_hash": "h",
		"lint": map[string]any{"errors": []any{}, "warnings": []any{}}, "notes": nil, "source": "api",
		"created_at": "2026-10-05T10:00:00Z", "published_at": "2026-10-05T10:00:00Z",
	}
	f.on("POST", "/v1/programs", writeJSON(409, map[string]any{"error": map[string]any{
		"code": "validation_failed", "message": "exists", "request_id": "req_1", "details": map[string]any{"program_id": "prg_1"},
	}}))
	f.on("POST", "/v1/programs/prg_1/versions", writeJSON(201, version))
	f.on("GET", "/v1/programs/prg_1", writeJSON(200, program))
	pushed, err := c.Programs.Push(context.Background(), map[string]any{"kaab": "1", "name": "demo", "steps": []any{}}, PushOptions{Notes: "second"})
	if err != nil {
		t.Fatal(err)
	}
	if pushed.Created || pushed.Version == nil || pushed.Version.Version != 2 || pushed.Program.LatestVersion != 2 {
		t.Fatalf("pushed %+v", pushed)
	}
	var got []string
	for _, r := range f.requests {
		got = append(got, r.Method+" "+r.Path)
	}
	want := []string{"POST /v1/programs", "POST /v1/programs/prg_1/versions", "GET /v1/programs/prg_1"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("requests %v, want %v", got, want)
	}

	// A new program costs one request.
	f2, c2, _ := newFake(t)
	f2.on("POST", "/v1/programs", writeJSON(201, program))
	if pushed, err := c2.Programs.Push(context.Background(), map[string]any{"kaab": "1", "name": "demo", "steps": []any{}}, PushOptions{}); err != nil || !pushed.Created || len(f2.requests) != 1 {
		t.Fatalf("pushed %+v, err %v, requests %d", pushed, err, len(f2.requests))
	}
}
