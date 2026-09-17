package miniship_test

// MINISHIP.md is the one place that says where the miniship line sits on
// upstream and what it changes there. miniship-cloud's scheduled check reads
// its fork point to decide which upstream releases and advisories are news, so
// a record that cannot be read, or that is wrong, is a fix nobody is told about.

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/prest/prest/v2/miniship"
)

// theRecord reads the fork's own MINISHIP.md, at the repository root.
func theRecord(t *testing.T) miniship.Record {
	t.Helper()
	text, err := os.ReadFile("../MINISHIP.md")
	require.NoError(t, err)
	record, err := miniship.Parse(string(text))
	require.NoError(t, err)
	return record
}

func TestTheRecordNamesAnUpstreamTagAndItsCommit(t *testing.T) {
	record := theRecord(t)
	require.Regexp(t, regexp.MustCompile(`^v\d+\.\d+\.\d+$`), record.ForkPoint.Tag)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{40}$`), record.ForkPoint.Commit)
}

// git runs git in the repository this test was checked out in. The record is
// about this branch's history, so a checkout without it is a failure to say
// so, not a skip: CI checks out with fetch-depth 0 for this reason.
func git(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s\n"+
		"these tests read the branch's history; a shallow clone does not have it (fetch-depth: 0)",
		strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

func TestTheForkPointIsUnderThisBranch(t *testing.T) {
	commit := theRecord(t).ForkPoint.Commit
	git(t, "merge-base", "--is-ancestor", commit, "HEAD")
}

func TestTheTagNamesTheRecordedCommit(t *testing.T) {
	point := theRecord(t).ForkPoint
	require.Equal(t, point.Commit, git(t, "rev-parse", "--verify", point.Tag+"^{commit}"),
		"MINISHIP.md says %s is %s", point.Tag, point.Commit)
}

// A change on the branch that no patch names is a patch nobody recorded, and
// the next rebase is where it gets lost.
func TestEveryPathTheBranchChangesIsClaimedByAPatch(t *testing.T) {
	record := theRecord(t)
	changed := strings.Split(git(t, "diff", "--name-only", record.ForkPoint.Commit, "HEAD"), "\n")
	require.NotEmpty(t, changed)
	for _, path := range changed {
		require.NotEmptyf(t, record.Claiming(path),
			"%s differs from %s, and no patch in MINISHIP.md names it: add it to a patch's Paths line",
			path, record.ForkPoint.Tag)
	}
}

// A claim that matches nothing is a patch the record still lists after it
// went, or a path spelled wrong.
func TestEveryClaimedPathIsChangedByTheBranch(t *testing.T) {
	record := theRecord(t)
	changed := strings.Split(git(t, "diff", "--name-only", record.ForkPoint.Commit, "HEAD"), "\n")
	for _, patch := range record.Patches {
		for _, claim := range patch.Paths {
			matched := false
			for _, path := range changed {
				matched = matched || miniship.Claims(claim, path)
			}
			require.Truef(t, matched, "patch %d claims %s, which does not differ from %s",
				patch.Number, claim, record.ForkPoint.Tag)
		}
	}
}

func TestThePatchesAreNumberedInOrderAndEachSaysWhy(t *testing.T) {
	record := theRecord(t)
	require.NotEmpty(t, record.Patches)
	for i, patch := range record.Patches {
		require.Equal(t, i+1, patch.Number, "patches are numbered 1, 2, 3… with no gap or repeat")
		require.NotEmptyf(t, patch.Title, "patch %d has no name", patch.Number)
		require.NotEmptyf(t, patch.Why, "patch %d says no why", patch.Number)
		require.NotEmptyf(t, patch.Paths, "patch %d names no path", patch.Number)
	}
}

func TestTheSamplePatchesAreRead(t *testing.T) {
	record, err := miniship.Parse(sample)
	require.NoError(t, err)
	require.Equal(t, []miniship.Patch{
		{
			Number: 1,
			Title:  "CI",
			Why:    "the line runs its own check.",
			Paths:  []string{".github/workflows/miniship.yml"},
		},
		{
			Number: 2,
			Title:  "Two routes",
			Why:    "the one door the internet reaches is small. It keeps the table read and the health address.",
			Paths:  []string{"router/router.go", "router/surface_test.go", "app/"},
		},
	}, record.Patches)
	require.Equal(t, []int{2}, numbers(record.Claiming("app/surface_test.go")))
	require.Equal(t, []int{2}, numbers(record.Claiming("router/router.go")))
	require.Empty(t, record.Claiming("router/router_test.go"))
	require.Empty(t, record.Claiming("application/main.go"))
}

func TestTheSampleVersionIsItsTagOnTheMinishipLine(t *testing.T) {
	record, err := miniship.Parse(sample)
	require.NoError(t, err)
	require.Equal(t, "2.4.2+miniship", record.Version())
}

// A sentence after the Paths line is prose about the patch, not more paths.
// The parser took every backticked word from the Paths line onward, so a patch
// could come to claim a path it does not change — or, worse, one another patch
// does change, which both record tests would then pass.
func TestASentenceAfterThePathsLineIsNotMorePaths(t *testing.T) {
	record, err := miniship.Parse(strings.Replace(sample,
		"   `app/`\n",
		"   `app/`\n   It also explains why `router/router.go` shrank.\n", 1))
	require.NoError(t, err)
	require.Equal(t, []string{"router/router.go", "router/surface_test.go", "app/"},
		record.Patches[1].Paths)
	require.Contains(t, record.Patches[1].Why, "shrank")
}

// A numbered line in the section that is not an entry is somebody's typo, and
// the entry it should have been is then silently gone: its Paths leak onto the
// patch above it, and only an interior one leaves a gap in the numbering.
func TestANumberedLineThatIsNotAnEntryIsRefused(t *testing.T) {
	_, err := miniship.Parse(strings.Replace(sample,
		"2. **Two routes** —", "2. Two routes -", 1))
	require.ErrorContains(t, err, "2.")
}

// An en dash is the same punctuation as far as a reader is concerned.
func TestAnEnDashSeparatesAnEntryTheSameWay(t *testing.T) {
	record, err := miniship.Parse(strings.ReplaceAll(sample, " — ", " – "))
	require.NoError(t, err)
	require.Len(t, record.Patches, 2)
	require.Equal(t, "the line runs its own check.", record.Patches[0].Why)
}

func TestAPatchWithNoPathsLineIsRefused(t *testing.T) {
	_, err := miniship.Parse(strings.Replace(sample, "   Paths: `.github/workflows/miniship.yml`\n", "", 1))
	require.ErrorContains(t, err, "patch 1")
}

func numbers(patches []miniship.Patch) []int {
	var out []int
	for _, patch := range patches {
		out = append(out, patch.Number)
	}
	return out
}

func TestAForkPointStatedTwiceIsRefused(t *testing.T) {
	_, err := miniship.Parse(sample + "\n| Upstream tag | `v2.5.0` |\n")
	require.ErrorContains(t, err, "Upstream tag")
}

func TestARecordWithNoForkPointIsRefused(t *testing.T) {
	_, err := miniship.Parse("# miniship's patch line\n")
	require.ErrorContains(t, err, "Upstream tag")
}

// sample is a record in the shape MINISHIP.md keeps, with values chosen here.
const sample = "# miniship's patch line\n\n" +
	"## The fork point\n\n" +
	"| | |\n| --- | --- |\n" +
	"| Upstream tag | `v2.4.2` |\n" +
	"| Upstream commit | `9070bda7e9ab6b6484e04a0983b8afd61c8315e6` |\n\n" +
	"## The patches\n\n" +
	"1. **CI** — the line runs its own check.\n" +
	"   Paths: `.github/workflows/miniship.yml`\n" +
	"2. **Two routes** — the one door the internet reaches is small.\n" +
	"   It keeps the table read and the health address.\n" +
	"   Paths: `router/router.go`, `router/surface_test.go`,\n" +
	"   `app/`\n\n" +
	"## Afterwards\n\n" +
	"Nothing here is a patch.\n"

func TestTheSampleForkPointIsRead(t *testing.T) {
	record, err := miniship.Parse(sample)
	require.NoError(t, err)
	require.Equal(t, miniship.ForkPoint{
		Tag:    "v2.4.2",
		Commit: "9070bda7e9ab6b6484e04a0983b8afd61c8315e6",
	}, record.ForkPoint)
}
