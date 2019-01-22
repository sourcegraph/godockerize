// +build go1.10

package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/build"
	"go/parser"
	"go/token"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/urfave/cli"
)

// Alpine doesn't do point releases, but if you are reading this, 3.8 downloads
// 3.8.1 or newer, which contains the security fix for this RCE:
// https://justi.cz/security/2018/09/13/alpine-apk-rce.html
const baseDockerImage = "alpine:3.8"

func main() {
	app := &cli.App{
		Name:    "godockerize",
		Usage:   "build Docker images from Go packages",
		Version: "0.0.2",
		Commands: []*cli.Command{
			{
				Name:        "build",
				Usage:       "build a Docker image from Go packages",
				ArgsUsage:   "[packages]",
				Description: "Build compiles and installs the packages by the import paths to /usr/local/bin\n   in the docker image. The first package is used as the entrypoint.",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "tag",
						Aliases: []string{"t"},
						Usage:   "output Docker image name and optionally a tag in the 'name:tag' format",
					},
					&cli.StringFlag{
						Name:  "base",
						Usage: "base Docker image name",
						Value: baseDockerImage,
					},
					&cli.StringSliceFlag{
						Name:  "env",
						Usage: "additional environment variables for the Dockerfile",
					},
					&cli.StringSliceFlag{
						Name:  "go-build-flags",
						Usage: "additional flags to pass to go build",
					},
					&cli.BoolFlag{
						Name:  "dry-run",
						Usage: "only print generated Dockerfile",
					},
				},
				Action: doBuild,
			},
		},
	}
	if err := app.Run(os.Args); err != nil {
		fmt.Printf("error: %s\n", err)
		os.Exit(1)
	}
}

func doBuild(c *cli.Context) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}

	args := c.Args()
	if args.Len() < 1 {
		return errors.New(`"godockerize build" requires 1 or more arguments`)
	}

	tmpdir, err := ioutil.TempDir("", "godockerize")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpdir)

	var (
		packages, expose, repos, run, userDirs []string
		user, cmd                              string

		fset    = token.NewFileSet()
		env     = c.StringSlice("env")
		install = []string{"ca-certificates", "mailcap", "tini"} // mailcap is for /etc/mime.types
	)

	targetDir := ""
	for i, pkgName := range args.Slice() {
		pkg, err := build.Import(pkgName, wd, 0)
		if err != nil {
			return err
		}
		packages = append(packages, pkg.ImportPath)

		if i == 0 {
			targetDir = pkg.Dir
		}

		isFirstPackage := i == 0
		for _, name := range pkg.GoFiles {
			f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.ParseComments)
			if err != nil {
				return err
			}

			for _, cg := range f.Comments {
				for _, c := range cg.List {
					if strings.HasPrefix(c.Text, "//docker:") {
						parts := strings.SplitN(c.Text[len("//docker:"):], " ", 2)
						switch parts[0] {
						case "env":
							if isFirstPackage {
								env = append(env, strings.Fields(parts[1])...)
							} else {
								fmt.Printf("%s: ignoring env directive since %s is not the first package\n", fset.Position(c.Pos()), pkgName)
							}
						case "expose":
							if isFirstPackage {
								expose = append(expose, strings.Fields(parts[1])...)
							} else {
								fmt.Printf("%s: ignoring expose directive since %s is not the first package\n", fset.Position(c.Pos()), pkgName)
							}
						case "install":
							install = append(install, strings.Fields(parts[1])...)
						case "repository":
							repos = append(repos, strings.Fields(parts[1])...)
						case "run":
							run = append(run, parts[1])
						case "cmd":
							if isFirstPackage {
								if cmd != "" {
									return errors.New("cmd set twice")
								}
								cmd = parts[1]
							} else {
								fmt.Printf("%s: ignoring cmd directive since %s is not the first package\n", fset.Position(c.Pos()), pkgName)
							}
						case "user":
							userArgs := strings.Fields(parts[1])
							if isFirstPackage {
								if user != "" {
									return errors.New("user set twice")
								}
								user = userArgs[0]
								if len(userArgs) > 1 {
									userDirs = userArgs[1:]
								}
							} else {
								fmt.Printf("%s: ignoring user directive since %s is not the first package\n", fset.Position(c.Pos()), pkgName)
							}
						default:
							return fmt.Errorf("%s: invalid docker comment: %s", fset.Position(c.Pos()), c.Text)
						}
					}
				}
			}
		}
	}

	var dockerfile bytes.Buffer
	dockerfile.WriteString(`# This Dockerfile was generated from github.com/sourcegraph/godockerize. It
# was not written by a human, and as such looks janky. As you change this
# file, please don't be scared to make it more pleasant / remove hadolint
# ignores.

`)
	fmt.Fprintf(&dockerfile, "FROM %s\n", c.String("base"))

	repoUrls := []string{}
	for _, pkg := range install {
		if strings.HasSuffix(pkg, "@edge") {
			repoUrls = append(repoUrls,
				`echo "@edge http://dl-cdn.alpinelinux.org/alpine/edge/main" >> /etc/apk/repositories`,
				`echo "@edge http://dl-cdn.alpinelinux.org/alpine/edge/community" >> /etc/apk/repositories`)
			break
		}
	}
	for _, r := range repos {
		repoUrls = append(repoUrls,
			fmt.Sprintf(`echo "http://dl-cdn.alpinelinux.org/alpine/%s/main" >> /etc/apk/repositories`, r),
			fmt.Sprintf(`echo "http://dl-cdn.alpinelinux.org/alpine/%s/community" >> /etc/apk/repositories`, r))
	}
	if len(repoUrls) > 0 {
		fmt.Fprintf(&dockerfile, "RUN %s\n", strings.Join(repoUrls, " && \\\n    "))
	}
	if strings.HasPrefix(c.String("base"), "alpine") {
		// IMPORTANT: Alpine by default does not come with some packages that
		// are needed for working DNS to other containers on a user-defined
		// Docker network. Without installing this package, nslookup and Go etc
		// will fail to contact other Docker containers.
		// See https://github.com/sourcegraph/deploy-sourcegraph-docker/issues/1
		install = append(install, "bind-tools")
	}
	if len(install) != 0 {
		dockerfile.WriteString("# hadolint ignore=DL3018\n")
		fmt.Fprintf(&dockerfile, "RUN apk add --no-cache %s\n", strings.Join(sortedStringSet(install), " "))
	}
	if user != "" {
		runCmds := []string{fmt.Sprintf("addgroup -S %s && adduser -S -G %s -h /home/%s %s", user, user, user, user)}
		for _, userDir := range userDirs {
			runCmds = append(runCmds, fmt.Sprintf("mkdir -p %s && chown -R %s:%s %s", userDir, user, user, userDir))
		}
		fmt.Fprintf(&dockerfile, "RUN "+strings.Join(runCmds, " && ")+"\n")
	}
	for _, cmd := range run {
		fmt.Fprintf(&dockerfile, "RUN %s\n", cmd)
	}
	if len(env) != 0 {
		fmt.Fprintf(&dockerfile, "ENV %s\n", strings.Join(sortedStringSet(env), " "))
	}
	if len(expose) != 0 {
		fmt.Fprintf(&dockerfile, "EXPOSE %s\n", strings.Join(sortedStringSet(expose), " "))
	}
	if user != "" {
		fmt.Fprintf(&dockerfile, "USER %s\n", user)
	}
	if cmd != "" {
		fmt.Fprintf(&dockerfile, "CMD %s\n", cmd)
	}
	fmt.Fprintf(&dockerfile, "ENTRYPOINT [\"/sbin/tini\", \"--\", \"/usr/local/bin/%s\"]\n", path.Base(packages[0]))
	for _, importPath := range packages {
		fmt.Fprintf(&dockerfile, "COPY %s /usr/local/bin/\n", path.Base(importPath))
	}
	fmt.Print(dockerfile.String())
	fmt.Println()
	if err := ioutil.WriteFile(filepath.Join(targetDir, "Dockerfile"), dockerfile.Bytes(), 0644); err != nil {
		return err
	}

	relPackage := strings.TrimPrefix(packages[0], "github.com/sourcegraph/sourcegraph/")

	var buildsh bytes.Buffer
	fmt.Fprintf(&buildsh, `#!/usr/bin/env bash

# We want to build multiple go binaries, so we use a custom build step on CI.
cd $(dirname "${BASH_SOURCE[0]}")%s
set -ex

OUTPUT=%s
cleanup() {
    rm -rf "$OUTPUT"
}
trap cleanup EXIT

# Environment for building linux binaries
export GO111MODULE=on
export GOARCH=amd64
export GOOS=linux
export CGO_ENABLED=0

for pkg in %s; do
    go build -ldflags "-X github.com/sourcegraph/sourcegraph/pkg/version.version=$VERSION" -buildmode exe -tags dist -o $OUTPUT/$(basename $pkg) $pkg
done

docker build -f %s/Dockerfile -t $IMAGE $OUTPUT
`, strings.Repeat("/..", len(strings.Split(relPackage, "/"))), "`mktemp -d -t sgdockerbuild_XXXXXXX`", strings.Join(packages, " \\\n    "), relPackage)

	fmt.Print(buildsh.String())
	fmt.Println()
	if err := ioutil.WriteFile(filepath.Join(targetDir, "build.sh"), buildsh.Bytes(), 0755); err != nil {
		return err
	}

	return nil
}

func sortedStringSet(in []string) []string {
	set := make(map[string]struct{})
	for _, s := range in {
		set[s] = struct{}{}
	}
	var out []string
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
