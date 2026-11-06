package kaab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/kaabist/kaab-go/client"
)

// PushOptions configures Programs.Push.
type PushOptions struct {
	// Name is the program's name; the definition's "name" when empty.
	Name string
	// Publish publishes the new version (moves alias latest); the API's default (true) when nil.
	Publish *bool
	// Notes are a new version's notes; Description a new program's description.
	Notes       string
	Description string
}

// PushResult is what Programs.Push did.
type PushResult struct {
	// Created is true when the push created the program, false when it added a version.
	Created bool
	Program *client.Program
	Version *client.ProgramVersion
}

// PushFile pushes a program definition file (JSON): it creates the program when its name
// is new, else adds a version to it.
func (r *ProgramsService) PushFile(ctx context.Context, path string, opts PushOptions, reqOpts ...RequestOption) (*PushResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var definition map[string]any
	if err := json.Unmarshal(data, &definition); err != nil {
		return nil, fmt.Errorf("%s is not a JSON object: %w", path, err)
	}
	return r.Push(ctx, definition, opts, reqOpts...)
}

// Push is PushFile with the definition already in hand.
func (r *ProgramsService) Push(ctx context.Context, definition map[string]any, opts PushOptions, reqOpts ...RequestOption) (*PushResult, error) {
	name := opts.Name
	if name == "" {
		name, _ = definition["name"].(string)
	}
	if name == "" {
		return nil, fmt.Errorf("kaab: the definition has no name: set PushOptions.Name")
	}
	named := make(map[string]any, len(definition)+1)
	for k, v := range definition {
		named[k] = v
	}
	named["name"] = name

	// Create first: a program that already exists answers 409 with its id, and the definition
	// becomes its next version. Two pushes at once cannot both create, and a new program costs
	// one request.
	create := map[string]any{"name": name, "definition": named}
	if opts.Publish != nil {
		create["publish"] = *opts.Publish
	}
	if opts.Description != "" {
		create["description"] = opts.Description
	}
	createBody, err := Body[client.CreateProgramJSONBody](create)
	if err != nil {
		return nil, err
	}
	program, err := r.Create(ctx, createBody, reqOpts...)
	if err == nil {
		return &PushResult{Created: true, Program: program}, nil
	}
	programID, exists := existingProgram(err)
	if !exists {
		return nil, err
	}
	version := map[string]any{"definition": named}
	if opts.Publish != nil {
		version["publish"] = *opts.Publish
	}
	if opts.Notes != "" {
		version["notes"] = opts.Notes
	}
	body, err := Body[client.CreateProgramVersionJSONBody](version)
	if err != nil {
		return nil, err
	}
	added, err := r.Versions.Create(ctx, programID, body, reqOpts...)
	if err != nil {
		return nil, err
	}
	// Deliberate: the result's aliases include the version just published.
	program, err = r.Get(ctx, programID, reqOpts...)
	if err != nil {
		return nil, err
	}
	return &PushResult{Created: false, Program: program, Version: added}, nil
}

// existingProgram is the id a 409 names when the program being created already exists.
func existingProgram(err error) (string, bool) {
	var kerr *Error
	if !asError(err, &kerr) || kerr.Status != 409 {
		return "", false
	}
	id, ok := kerr.Details["program_id"].(string)
	return id, ok && id != ""
}
