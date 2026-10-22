package artifactpublisher_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pax-beehive/paxd/internal/artifactfetch"
	"github.com/pax-beehive/paxd/internal/artifactpublisher"
	"github.com/pax-beehive/paxd/internal/control"
	"github.com/pax-beehive/paxd/internal/daemonstore"
)

const fetchPublicationID = "apub_0123456789abcdef0123456789abcdef"

type fetchFixture struct {
	store   *daemonstore.Store
	home    string
	root    string
	wakes   int
	service *artifactpublisher.FetchService
}

func newFetchFixture(t *testing.T, targets artifactpublisher.TargetResolver) *fetchFixture {
	t.Helper()
	f := &fetchFixture{
		store: openArtifactStore(t, filepath.Join(t.TempDir(), "paxd.db")),
		home:  t.TempDir(),
		root:  filepath.Join(t.TempDir(), "spool"),
	}
	f.service = artifactpublisher.NewFetchService(artifactpublisher.FetchOptions{
		Store:      f.store,
		Targets:    targets,
		RootDir:    f.root,
		HomeDir:    f.home,
		Now:        fixedArtifactTime,
		OnAccepted: func() { f.wakes++ },
	})
	return f
}

func (f *fetchFixture) write(t *testing.T, rel, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(f.home, filepath.FromSlash(rel))
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func fetchCommand(path string) control.FetchArtifactCommand {
	return control.FetchArtifactCommand{
		PublicationID: fetchPublicationID, AgentID: "agent_1", SessionID: "session_1", Path: path,
	}
}

var remoteHome = control.Source{Kind: control.SourceRemote, RemoteID: "remote_home"}

func TestFetchGivenBoundRemoteAndFetchableFileThenPersistsSnapshottingJobWithoutCopying(t *testing.T) {
	f := newFetchFixture(t, staticAcceptTarget{remoteID: "remote_home"})
	path := f.write(t, "proj/x.md", "hello", 0o644)

	result, err := f.service.Fetch(context.Background(), remoteHome, fetchCommand(filepath.Join(f.home, "proj", ".", "x.md")))

	require.NoError(t, err)
	assert.Equal(t, control.ArtifactFetchResult{
		PublicationID: fetchPublicationID, Status: artifactpublisher.StatusSnapshotting,
		Filename: "x.md", SizeBytes: 5,
	}, result)
	job, err := f.store.GetArtifactPublishJob(context.Background(), fetchPublicationID)
	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.OriginUserFetch, job.Origin)
	assert.Equal(t, path, job.SourcePath)
	assert.Equal(t, artifactpublisher.StatusSnapshotting, job.Status)
	assert.Equal(t, filepath.Join(f.root, fetchPublicationID, "content"), job.SpoolPath)
	assert.Equal(t, int64(5), job.SizeBytes)
	assert.Equal(t, "remote_home", job.RemoteID)
	assert.Equal(t, "agent_1", job.AgentID)
	assert.Equal(t, "session_1", job.SessionID)
	assert.Equal(t, "x.md", job.SourceFilename)
	assert.NoFileExists(t, job.SpoolPath, "the copy happens in the worker, after the ack")
	assert.Equal(t, 1, f.wakes)
}

func TestFetchGivenAgentBoundToAnotherRemoteThenUnauthorized(t *testing.T) {
	for name, targets := range map[string]artifactpublisher.TargetResolver{
		"other remote":   staticAcceptTarget{remoteID: "remote_other"},
		"resolver error": staticAcceptTarget{err: errors.New("no enabled manager connection")},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFetchFixture(t, targets)
			path := f.write(t, "x.md", "hello", 0o644)

			_, err := f.service.Fetch(context.Background(), remoteHome, fetchCommand(path))

			assertControlCode(t, err, "unauthorized")
			_, getErr := f.store.GetArtifactPublishJob(context.Background(), fetchPublicationID)
			assert.ErrorIs(t, getErr, daemonstore.ErrNotFound)
			assert.Zero(t, f.wakes)
		})
	}
}

func TestFetchGivenLocalSourceThenUnauthorized(t *testing.T) {
	f := newFetchFixture(t, staticAcceptTarget{remoteID: "remote_home"})
	path := f.write(t, "x.md", "hello", 0o644)

	for _, src := range []control.Source{{Kind: control.SourceLocal}, {Kind: control.SourceRemote}} {
		_, err := f.service.Fetch(context.Background(), src, fetchCommand(path))
		assertControlCode(t, err, "unauthorized")
	}
}

func TestFetchGivenDeniedPathThenRejectsWithRuleAndCreatesNoJob(t *testing.T) {
	f := newFetchFixture(t, staticAcceptTarget{remoteID: "remote_home"})
	path := f.write(t, ".ssh/id_rsa", "key", 0o644)

	_, err := f.service.Fetch(context.Background(), remoteHome, fetchCommand(path))

	var controlErr control.ControlError
	require.ErrorAs(t, err, &controlErr)
	assert.Equal(t, artifactfetch.CodeHiddenPathDenied, controlErr.Code)
	assert.Equal(t, artifactfetch.RuleHiddenPath, controlErr.Target)
	assert.NotEmpty(t, controlErr.Message)
	_, getErr := f.store.GetArtifactPublishJob(context.Background(), fetchPublicationID)
	assert.ErrorIs(t, getErr, daemonstore.ErrNotFound)
	assert.Zero(t, f.wakes)
}

func TestFetchGivenRepeatedPublicationThenReturnsCurrentStatusOrConflict(t *testing.T) {
	f := newFetchFixture(t, staticAcceptTarget{remoteID: "remote_home"})
	path := f.write(t, "x.md", "hello", 0o644)
	_, err := f.service.Fetch(context.Background(), remoteHome, fetchCommand(path))
	require.NoError(t, err)
	job, err := f.store.GetArtifactPublishJob(context.Background(), fetchPublicationID)
	require.NoError(t, err)
	job.Status = artifactpublisher.StatusUploading
	require.NoError(t, f.store.SaveArtifactPublishJob(context.Background(), job))
	// A later policy change does not break idempotent retries of an accepted fetch.
	require.NoError(t, os.Chmod(path, 0o600))

	again, err := f.service.Fetch(context.Background(), remoteHome, fetchCommand(path))

	require.NoError(t, err)
	assert.Equal(t, artifactpublisher.StatusUploading, again.Status)
	assert.Equal(t, "x.md", again.Filename)
	assert.Equal(t, int64(5), again.SizeBytes)

	for name, mutate := range map[string]func(*control.FetchArtifactCommand){
		"different agent":   func(c *control.FetchArtifactCommand) { c.AgentID = "agent_2" },
		"different session": func(c *control.FetchArtifactCommand) { c.SessionID = "session_2" },
		"different path":    func(c *control.FetchArtifactCommand) { c.Path = filepath.Join(f.home, "y.md") },
	} {
		t.Run(name, func(t *testing.T) {
			cmd := fetchCommand(path)
			mutate(&cmd)
			targets := staticAcceptTarget{remoteID: "remote_home"}
			service := artifactpublisher.NewFetchService(artifactpublisher.FetchOptions{
				Store: f.store, Targets: targets, RootDir: f.root, HomeDir: f.home,
			})
			_, err := service.Fetch(context.Background(), remoteHome, cmd)
			assertControlCode(t, err, "conflict")
		})
	}
}

func TestFetchGivenExistingAgentPublicationIDThenConflict(t *testing.T) {
	f := newFetchFixture(t, staticAcceptTarget{remoteID: "remote_home"})
	path := f.write(t, "x.md", "hello", 0o644)
	now := fixedArtifactTime()
	require.NoError(t, f.store.CreateArtifactPublishJob(context.Background(), daemonstore.ArtifactPublishJob{
		PublicationID: fetchPublicationID, AgentID: "agent_1", SessionID: "session_1",
		SourceFilename: "x.md", SpoolPath: "/spool/x", Status: artifactpublisher.StatusAccepted,
		RemoteID: "remote_home", CreatedAt: now, UpdatedAt: now,
	}))

	_, err := f.service.Fetch(context.Background(), remoteHome, fetchCommand(path))

	assertControlCode(t, err, "conflict")
}

func TestFetchServiceCheckReportsAllowedAndDenied(t *testing.T) {
	f := newFetchFixture(t, staticAcceptTarget{remoteID: "remote_home"})
	allowedPath := f.write(t, "proj/x.md", "hello", 0o644)
	ownerOnly := f.write(t, "proj/private.md", "hello", 0o600)

	allowed, err := f.service.Check(context.Background(), remoteHome, control.CheckArtifactFetchQuery{Path: allowedPath})
	require.NoError(t, err)
	assert.True(t, allowed.Allowed)
	require.NotNil(t, allowed.SizeBytes)
	assert.Equal(t, int64(5), *allowed.SizeBytes)
	assert.Equal(t, "x.md", allowed.Filename)
	assert.Equal(t, allowedPath, allowed.Path)
	assert.Nil(t, allowed.Denial)

	denied, err := f.service.Check(context.Background(), remoteHome, control.CheckArtifactFetchQuery{Path: ownerOnly})
	require.NoError(t, err)
	assert.False(t, denied.Allowed)
	assert.Nil(t, denied.SizeBytes)
	assert.Equal(t, "private.md", denied.Filename)
	require.NotNil(t, denied.Denial)
	assert.Equal(t, artifactfetch.CodeOwnerOnlyDenied, denied.Denial.Code)
	assert.Equal(t, artifactfetch.RuleOwnerOnly, denied.Denial.Rule)
	assert.Contains(t, denied.Denial.Message, "chmod 644")
}

func assertControlCode(t *testing.T, err error, code string) {
	t.Helper()
	var controlErr control.ControlError
	require.ErrorAs(t, err, &controlErr)
	assert.Equal(t, code, controlErr.Code)
}
