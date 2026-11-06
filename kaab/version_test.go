package kaab

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
)

// The SDK follows the API's major.minor. The OpenAPI document lives in the monorepo; the public
// mirror (github.com/kaabist/kaab-go) has no copy, so there the check is skipped.
func TestVersionFollowsTheAPIMinor(t *testing.T) {
	data, err := os.ReadFile("../../../packages/contracts/openapi/v1.json")
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("the OpenAPI document is not here: this is the mirror")
	}
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Info struct {
			Version string `json:"version"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(Version, ".", 3)
	if len(parts) != 3 || parts[0]+"."+parts[1] != doc.Info.Version {
		t.Fatalf("Version %s does not follow the API's %s", Version, doc.Info.Version)
	}
}
