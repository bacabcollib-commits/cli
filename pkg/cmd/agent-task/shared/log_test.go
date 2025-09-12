package shared

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFollow(t *testing.T) {
	tests := []struct {
		name string
		log  string
		want string
	}{
		{
			name: "sample log 1",
			log:  "testdata/sample-log-1.txt",
			want: "testdata/sample-log-1.want.txt",
		},
		{
			name: "sample log 2",
			log:  "testdata/sample-log-2.txt",
			want: "testdata/sample-log-2.want.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := os.ReadFile(tt.log)
			require.NoError(t, err)

			lines := slices.DeleteFunc(strings.Split(string(raw), "\n"), func(line string) bool {
				return line == ""
			})

			var hits int
			fetcher := func() ([]byte, error) {
				hits++
				if hits > len(lines) {
					require.FailNow(t, "too many API calls")
				}
				return []byte(strings.Join(lines[0:hits], "\n\n")), nil
			}

			ios, _, stdout, _ := iostreams.Test()

			err = NewLogRenderer().Follow(fetcher, stdout, ios)
			require.NoError(t, err)

			want, err := os.ReadFile(tt.want)
			require.NoError(t, err)

			// // Temp for updating tests
			// os.WriteFile(tt.log+".got", stdout.Bytes(), 0644)

			assert.Equal(t, string(want), stdout.String())
		})
	}
}
