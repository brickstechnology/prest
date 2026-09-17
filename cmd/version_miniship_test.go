package cmd

// Someone holding only a rest image can ask the binary which upstream release
// it carries. The answer is MINISHIP.md's fork point, on the miniship line.

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/miniship"
)

func TestVersionNamesTheRecordedUpstreamTagOnTheMinishipLine(t *testing.T) {
	text, err := os.ReadFile("../MINISHIP.md")
	require.NoError(t, err)
	record, err := miniship.Parse(string(text))
	require.NoError(t, err)

	printed := stdoutOf(t, func() { versionCmd.Run(versionCmd, nil) })

	require.Truef(t, strings.HasSuffix(strings.TrimSpace(printed), " "+record.Version()),
		"prestd version printed %q; MINISHIP.md's fork point is %s, so it should end in %s",
		printed, record.ForkPoint.Tag, record.Version())
}

func stdoutOf(t *testing.T, run func()) string {
	t.Helper()
	read, write, err := os.Pipe()
	require.NoError(t, err)
	saved := os.Stdout
	os.Stdout = write
	defer func() { os.Stdout = saved }()
	run()
	require.NoError(t, write.Close())
	out, err := io.ReadAll(read)
	require.NoError(t, err)
	return string(out)
}
