package pipelines

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/databricks/cli/bundle"
	"github.com/databricks/cli/bundle/config"
	"github.com/databricks/cli/libs/vfs"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveRefreshSelectors(t *testing.T) {
	tests := []struct {
		name      string
		selectors []string
		tables    []string
		paths     []string
		err       string
	}{
		{name: "no selection"},
		{
			name:      "table identifiers",
			selectors: []string{"table1", "schema.table2", "schema.sql", "catalog.schema.sql", "`schema`.`table/with/slashes`", "`schema`.`sql`"},
			tables:    []string{"table1", "schema.table2", "schema.sql", "catalog.schema.sql", "`schema`.`table/with/slashes`", "`schema`.`sql`"},
		},
		{
			name:      "quoted table with the same name as a file",
			selectors: []string{"`src/query.sql`"},
			tables:    []string{"`src/query.sql`"},
		},
		{
			name:      "mixed table and file selectors",
			selectors: []string{"table1", "src/query.sql", "src/module.py"},
			tables:    []string{"table1"},
			paths:     []string{"/Workspace/Shared/project/files/src/query.sql", "/Workspace/Shared/project/files/src/module.py"},
		},
		{
			name:      "notebook extension removed",
			selectors: []string{"src/notebook.py", "src/jupyter.ipynb"},
			paths:     []string{"/Workspace/Shared/project/files/src/notebook", "/Workspace/Shared/project/files/src/jupyter"},
		},
		{
			name:      "workspace path",
			selectors: []string{"/Workspace/Shared/remote.sql"},
			paths:     []string{"/Workspace/Shared/remote.sql"},
		},
		{name: "missing relative file", selectors: []string{"src/missing.sql"}, err: `source file "src/missing.sql" not found`},
		{name: "explicit missing filename", selectors: []string{"./missing.sql"}, err: `source file "./missing.sql" not found`},
		{name: "directory", selectors: []string{"src"}, err: "is a directory"},
		{name: "excluded file", selectors: []string{"src/excluded.sql"}, err: "is excluded from bundle sync"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0o700))
			for name, content := range map[string]string{
				"query.sql":     "SELECT 1;\n",
				"module.py":     "print('file')\n",
				"notebook.py":   "# Databricks notebook source\nprint('notebook')\n",
				"jupyter.ipynb": `{"cells":[],"metadata":{},"nbformat":4,"nbformat_minor":5}`,
				"excluded.sql":  "SELECT 2;\n",
			} {
				require.NoError(t, os.WriteFile(filepath.Join(root, "src", name), []byte(content), 0o600))
			}
			t.Chdir(root)
			b := refreshTestBundle(root)
			b.Config.Sync.Exclude = []string{"src/excluded.sql"}
			tables, paths, err := resolveRefreshSelectors(t.Context(), b, tt.selectors)
			if tt.err != "" {
				require.ErrorContains(t, err, tt.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.tables, tables)
			assert.Equal(t, tt.paths, paths)
		})
	}
}

func refreshTestBundle(root string) *bundle.Bundle {
	return &bundle.Bundle{
		BundleRootPath: root,
		SyncRootPath:   root,
		SyncRoot:       vfs.MustNew(root),
		WorktreeRoot:   vfs.MustNew(root),
		Config: config.Root{
			Bundle:    config.Bundle{Name: "test", Target: "default"},
			Workspace: config.Workspace{FilePath: "/Workspace/Shared/project/files"},
			Sync:      config.Sync{Paths: []string{"."}},
		},
	}
}

func TestResolveRefreshSelectorsFromSubdirectory(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "project", "src"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "shared.sql"), []byte("SELECT 1;\n"), 0o600))
	t.Chdir(filepath.Join(root, "project", "src"))
	b := refreshTestBundle(root)
	b.BundleRootPath = filepath.Join(root, "project")
	for _, selector := range []string{"../../shared.sql", filepath.Join(root, "shared.sql")} {
		tables, paths, err := resolveRefreshSelectors(t.Context(), b, []string{selector})
		require.NoError(t, err)
		assert.Empty(t, tables)
		assert.Equal(t, []string{"/Workspace/Shared/project/files/shared.sql"}, paths)
	}
}

func TestResolveRefreshSelectorsOutsideSyncRoot(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(root, "project"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "outside.sql"), []byte("SELECT 1;\n"), 0o600))
	t.Chdir(filepath.Join(root, "project"))
	_, _, err := resolveRefreshSelectors(t.Context(), refreshTestBundle(filepath.Join(root, "project")), []string{"../outside.sql"})
	require.ErrorContains(t, err, "is outside the bundle sync root")
}

func TestResolveRefreshSelectorsSourceLinked(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "query.sql"), []byte("SELECT 1;\n"), 0o600))
	t.Chdir(root)
	b := refreshTestBundle(root)
	enabled := true
	b.Config.Presets.SourceLinkedDeployment = &enabled
	_, paths, err := resolveRefreshSelectors(t.Context(), b, []string{"query.sql"})
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.ToSlash(filepath.Join(root, "query.sql"))}, paths)
}

func TestResolveRefreshSelectorsUnresolvedWorkspacePath(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "query.sql"), []byte("SELECT 1;\n"), 0o600))
	t.Chdir(root)
	b := refreshTestBundle(root)
	b.Config.Workspace.FilePath = "${resources.internal_immutable_snapshots.immutable.full_path}/files"
	_, _, err := resolveRefreshSelectors(t.Context(), b, []string{"query.sql"})
	require.ErrorContains(t, err, "use its absolute /Workspace/ path")
}
