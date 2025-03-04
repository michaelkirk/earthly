package buildcontext

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/earthly/earthly/analytics"
	"github.com/earthly/earthly/cleanup"
	"github.com/earthly/earthly/conslogging"
	"github.com/earthly/earthly/domain"
	"github.com/earthly/earthly/features"
	"github.com/earthly/earthly/outmon"
	"github.com/earthly/earthly/util/gitutil"
	"github.com/earthly/earthly/util/llbutil"
	"github.com/earthly/earthly/util/llbutil/llbfactory"
	"github.com/earthly/earthly/util/llbutil/pllb"
	"github.com/earthly/earthly/util/platutil"
	"github.com/earthly/earthly/util/stringutil"
	"github.com/earthly/earthly/util/syncutil/synccache"

	"github.com/moby/buildkit/client/llb"
	gwclient "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/pkg/errors"
)

const (
	defaultGitImage = "alpine/git:v2.30.1"
)

type gitResolver struct {
	cleanCollection *cleanup.Collection

	projectCache   *synccache.SyncCache // "gitURL#gitRef" -> *resolvedGitProject
	buildFileCache *synccache.SyncCache // project ref -> local path
	gitLookup      *GitLookup
	console        conslogging.ConsoleLogger
}

type resolvedGitProject struct {
	// hash is the git hash.
	hash string
	// shortHash is the short git hash.
	shortHash string
	// branches is the git branches.
	branches []string
	// tags is the git tags.
	tags []string
	// ts is the git commit timestamp.
	ts        string
	author    string
	coAuthors []string
	// state is the state holding the git files.
	state pllb.State
}

func (gr *gitResolver) resolveEarthProject(ctx context.Context, gwClient gwclient.Client, platr *platutil.Resolver, ref domain.Reference, featureFlagOverrides string) (*Data, error) {
	if !ref.IsRemote() {
		return nil, errors.Errorf("unexpected local reference %s", ref.String())
	}
	rgp, gitURL, subDir, err := gr.resolveGitProject(ctx, gwClient, platr, ref)
	if err != nil {
		return nil, err
	}

	var buildContextFactory llbfactory.Factory
	if _, isTarget := ref.(domain.Target); isTarget {
		// Restrict the resulting build context to the right subdir.
		if subDir == "." {
			// Optimization.
			buildContextFactory = llbfactory.PreconstructedState(rgp.state)
		} else {
			vm := &outmon.VertexMeta{
				TargetName: ref.String(),
				Internal:   true,
			}
			copyState, err := llbutil.CopyOp(ctx,
				rgp.state, []string{subDir}, platr.Scratch(), "./", false, false, false, "root:root", nil, false, false, false,
				llb.WithCustomNamef("%sCOPY git context %s", vm.ToVertexPrefix(), ref.String()))
			if err != nil {
				return nil, errors.Wrap(err, "copyOp failed in resolveEarthProject")
			}
			buildContextFactory = llbfactory.PreconstructedState(copyState)
		}
	}
	// Else not needed: Commands don't come with a build context.

	key := ref.ProjectCanonical()
	isDockerfile := strings.HasPrefix(ref.GetName(), DockerfileMetaTarget)
	if isDockerfile {
		// Different key for dockerfiles to include the dockerfile name itself.
		key = ref.StringCanonical()
	}
	localBuildFileValue, err := gr.buildFileCache.Do(ctx, key, func(ctx context.Context, _ interface{}) (interface{}, error) {
		earthfileTmpDir, err := os.MkdirTemp(os.TempDir(), "earthly-git")
		if err != nil {
			return nil, errors.Wrap(err, "create temp dir for Earthfile")
		}
		gr.cleanCollection.Add(func() error {
			return os.RemoveAll(earthfileTmpDir)
		})
		gitState, err := llbutil.StateToRef(
			ctx, gwClient, rgp.state, false,
			platr.SubResolver(platutil.NativePlatform), nil)
		if err != nil {
			return nil, errors.Wrap(err, "state to ref git meta")
		}
		bf, err := detectBuildFileInRef(ctx, ref, gitState, subDir)
		if err != nil {
			return nil, err
		}
		bfBytes, err := gitState.ReadFile(ctx, gwclient.ReadRequest{
			Filename: bf,
		})
		if err != nil {
			return nil, errors.Wrap(err, "read build file")
		}
		localBuildFilePath := filepath.Join(earthfileTmpDir, path.Base(bf))
		err = os.WriteFile(localBuildFilePath, bfBytes, 0700)
		if err != nil {
			return nil, errors.Wrapf(err, "write build file to tmp dir at %s", localBuildFilePath)
		}
		var ftrs *features.Features
		if isDockerfile {
			ftrs = new(features.Features)
		} else {
			ftrs, err = parseFeatures(localBuildFilePath, featureFlagOverrides, ref.ProjectCanonical(), gr.console)
			if err != nil {
				return nil, err
			}
		}
		return &buildFile{
			path: localBuildFilePath,
			ftrs: ftrs,
		}, nil
	})
	if err != nil {
		return nil, err
	}
	localBuildFile := localBuildFileValue.(*buildFile)

	// TODO: Apply excludes / .earthignore.
	return &Data{
		BuildFilePath:       localBuildFile.path,
		BuildContextFactory: buildContextFactory,
		GitMetadata: &gitutil.GitMetadata{
			BaseDir:   "",
			RelDir:    subDir,
			RemoteURL: gitURL,
			Hash:      rgp.hash,
			ShortHash: rgp.shortHash,
			Branch:    rgp.branches,
			Tags:      rgp.tags,
			Timestamp: rgp.ts,
			Author:    rgp.author,
			CoAuthors: rgp.coAuthors,
		},
		Features: localBuildFile.ftrs,
	}, nil
}

func (gr *gitResolver) resolveGitProject(ctx context.Context, gwClient gwclient.Client, platr *platutil.Resolver, ref domain.Reference) (rgp *resolvedGitProject, gitURL string, subDir string, finalErr error) {
	gitRef := ref.GetTag()

	var err error
	var keyScans []string
	gitURL, subDir, keyScans, err = gr.gitLookup.GetCloneURL(ref.GetGitURL())
	if err != nil {
		return nil, "", "", errors.Wrap(err, "failed to get url for cloning")
	}
	analytics.Count("gitResolver.resolveEarthProject", analytics.RepoHashFromCloneURL(gitURL))

	// Check the cache first.
	cacheKey := fmt.Sprintf("%s#%s", gitURL, gitRef)
	rgpValue, err := gr.projectCache.Do(ctx, cacheKey, func(ctx context.Context, k interface{}) (interface{}, error) {
		// Copy all Earthfile, build.earth and Dockerfile files.
		vm := &outmon.VertexMeta{
			TargetName: cacheKey,
			Internal:   true,
		}
		gitOpts := []llb.GitOption{
			llb.WithCustomNamef("%sGIT CLONE %s", vm.ToVertexPrefix(), stringutil.ScrubCredentials(gitURL)),
			llb.KeepGitDir(),
		}
		if len(keyScans) > 0 {
			gitOpts = append(gitOpts, llb.KnownSSHHosts(strings.Join(keyScans, "\n")))
		}

		gitState := llb.Git(gitURL, gitRef, gitOpts...)
		opImg := pllb.Image(
			defaultGitImage, llb.MarkImageInternal, llb.ResolveModePreferLocal,
			llb.Platform(platr.LLBNative()))

		// Get git hash.
		gitHashOpts := []llb.RunOption{
			llb.Args([]string{
				"/bin/sh", "-c",
				"git rev-parse HEAD >/dest/git-hash ; " +
					"git rev-parse --short=8 HEAD >/dest/git-short-hash ; " +
					"git rev-parse --abbrev-ref HEAD >/dest/git-branch  || touch /dest/git-branch ; " +
					"git describe --exact-match --tags >/dest/git-tags || touch /dest/git-tags ; " +
					"git log -1 --format=%ct >/dest/git-ts || touch /dest/git-ts ; " +
					"git log -1 --format=%ae >/dest/git-author || touch /dest/git-author ; " +
					"git log -1 --format=%b >/dest/git-body || touch /dest/git-body ; " +
					"",
			}),
			llb.Dir("/git-src"),
			llb.ReadonlyRootFS(),
			llb.AddMount("/git-src", gitState, llb.Readonly),
			llb.WithCustomNamef("%sGET GIT META %s", vm.ToVertexPrefix(), ref.ProjectCanonical()),
		}
		gitHashOp := opImg.Run(gitHashOpts...)
		gitMetaState := gitHashOp.AddMount("/dest", platr.Scratch())

		noCache := false // TODO figure out if we want to propagate --no-cache here
		gitMetaRef, err := llbutil.StateToRef(
			ctx, gwClient, gitMetaState, noCache,
			platr.SubResolver(platutil.NativePlatform), nil)
		if err != nil {
			return nil, errors.Wrap(err, "state to ref git meta")
		}
		gitHashBytes, err := gitMetaRef.ReadFile(ctx, gwclient.ReadRequest{
			Filename: "git-hash",
		})
		if err != nil {
			return nil, errors.Wrap(err, "read git-hash")
		}
		gitShortHashBytes, err := gitMetaRef.ReadFile(ctx, gwclient.ReadRequest{
			Filename: "git-short-hash",
		})
		if err != nil {
			return nil, errors.Wrap(err, "read git-short-hash")
		}
		gitBranchBytes, err := gitMetaRef.ReadFile(ctx, gwclient.ReadRequest{
			Filename: "git-branch",
		})
		if err != nil {
			return nil, errors.Wrap(err, "read git-branch")
		}
		gitTagsBytes, err := gitMetaRef.ReadFile(ctx, gwclient.ReadRequest{
			Filename: "git-tags",
		})
		if err != nil {
			return nil, errors.Wrap(err, "read git-tags")
		}
		gitTsBytes, err := gitMetaRef.ReadFile(ctx, gwclient.ReadRequest{
			Filename: "git-ts",
		})
		if err != nil {
			return nil, errors.Wrap(err, "read git-ts")
		}
		gitAuthorBytes, err := gitMetaRef.ReadFile(ctx, gwclient.ReadRequest{
			Filename: "git-author",
		})
		if err != nil {
			return nil, errors.Wrap(err, "read git-author")
		}
		gitBodyBytes, err := gitMetaRef.ReadFile(ctx, gwclient.ReadRequest{
			Filename: "git-body",
		})
		if err != nil {
			return nil, errors.Wrap(err, "read git-body")
		}

		gitHash := strings.SplitN(string(gitHashBytes), "\n", 2)[0]
		gitShortHash := strings.SplitN(string(gitShortHashBytes), "\n", 2)[0]
		gitBranches := strings.SplitN(string(gitBranchBytes), "\n", 2)
		gitAuthor := strings.SplitN(string(gitAuthorBytes), "\n", 2)[0]
		gitCoAuthors := gitutil.ParseCoAuthorsFromBody(string(gitBodyBytes))
		var gitBranches2 []string
		for _, gitBranch := range gitBranches {
			if gitBranch != "" {
				gitBranches2 = append(gitBranches2, gitBranch)
			}
		}
		gitTags := strings.SplitN(string(gitTagsBytes), "\n", 2)
		var gitTags2 []string
		for _, gitTag := range gitTags {
			if gitTag != "" && gitTag != "HEAD" {
				gitTags2 = append(gitTags2, gitTag)
			}
		}
		gitTs := strings.SplitN(string(gitTsBytes), "\n", 2)[0]

		gitOpts = []llb.GitOption{
			llb.WithCustomNamef("[context %s] git context %s", stringutil.ScrubCredentials(gitURL), ref.StringCanonical()),
			llb.KeepGitDir(),
		}
		if len(keyScans) > 0 {
			gitOpts = append(gitOpts, llb.KnownSSHHosts(strings.Join(keyScans, "\n")))
		}

		rgp := &resolvedGitProject{
			hash:      gitHash,
			shortHash: gitShortHash,
			branches:  gitBranches2,
			tags:      gitTags2,
			ts:        gitTs,
			author:    gitAuthor,
			coAuthors: gitCoAuthors,
			state: pllb.Git(
				gitURL,
				gitHash,
				gitOpts...,
			),
		}
		go func() {
			// Add cache entries for the branch and for the tag (if any).
			if len(gitBranches2) > 0 {
				cacheKey3 := fmt.Sprintf("%s#%s", gitURL, gitBranches2[0])
				_ = gr.projectCache.Add(ctx, cacheKey3, rgp, nil)
			}
			if len(gitTags2) > 0 {
				cacheKey4 := fmt.Sprintf("%s#%s", gitURL, gitTags2[0])
				_ = gr.projectCache.Add(ctx, cacheKey4, rgp, nil)
			}
		}()
		return rgp, nil
	})
	if err != nil {
		return nil, "", "", err
	}
	rgp = rgpValue.(*resolvedGitProject)
	return rgp, gitURL, subDir, nil
}
