package main

import (
	"bufio"
	"flag"
	"fmt"
	"go/build"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/mattn/go-zglob"
	"github.com/otiai10/copy"
)

var (
	flags        = flag.NewFlagSet("modvendor", flag.ExitOnError)
	copyPatFlag  = flags.String("copy", "", "copy files matching glob pattern to ./vendor/ (ie. modvendor -copy=\"**/*.c **/*.h **/*.proto\")")
	fullCopyFlag = flags.Bool("fullcopy", true, "copy all project files to ./vendor/ (ie. modvendor -fullcopy=true")
	verboseFlag  = flags.Bool("v", false, "verbose output")
	includeFlag  = flags.String(
		"include",
		"",
		`specifies additional directories to copy into ./vendor/ which are not specified in ./vendor/modules.txt. Multiple directories can be included by comma separation e.g. -include:github.com/a/b/dir1,github.com/a/b/dir1/dir2`)
)

type Mod struct {
	ImportPath    string
	SourcePath    string
	Version       string
	SourceVersion string
	Dir           string          // full path, $GOPATH/pkg/mod/
	Pkgs          []string        // sub-pkg import paths
	VendorList    map[string]bool // files to vendor
}

func main() {
	err := flags.Parse(os.Args[1:])
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	// Ensure go.mod file exists and we're running from the project root,
	// and that ./vendor/modules.txt file exists.
	cwd, err := os.Getwd()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	if _, err := os.Stat(filepath.Join(cwd, "go.mod")); os.IsNotExist(err) {
		fmt.Println("Whoops, cannot find `go.mod` file")
		os.Exit(1)
	}
	modtxtPath := filepath.Join(cwd, "vendor", "modules.txt")
	if _, err := os.Stat(modtxtPath); os.IsNotExist(err) {
		fmt.Println("Whoops, cannot find vendor/modules.txt, first run `go mod vendor` and try again")
		os.Exit(1)
	}

	// Prepare vendor copy patterns
	copyPat := strings.Split(strings.TrimSpace(*copyPatFlag), " ")
	if len(copyPat) == 0 {
		fmt.Println("Whoops, -copy argument is empty, nothing to copy.")
		os.Exit(1)
	}
	additionalDirsToInclude := strings.Split(*includeFlag, ",")

	// Parse/process modules.txt file of pkgs
	f, _ := os.Open(modtxtPath)
	defer func() {
		_ = f.Close()
	}()

	scanner := bufio.NewScanner(f)
	scanner.Split(bufio.ScanLines)

	var mod *Mod
	var modules []*Mod

	for scanner.Scan() {
		line := scanner.Text()

		if line[0] == '#' {
			s := strings.Split(line, " ")

			// ignore patterns except for
			// - ordinary module
			//   # <mod> version
			// - replace
			//   # <mod> version => <mod1> version1
			// - replace with local version
			//   # <mod> version => <local path to mod1>
			if (len(s) != 6 && len(s) != 5 && len(s) != 3) ||
				s[1] == "explicit" {
				continue
			}

			// issue https://github.com/golang/go/issues/33848 added these,
			// see comments. I think we can get away with ignoring them.
			if s[2] == "=>" {
				continue
			}

			mod = &Mod{
				ImportPath: s[1],
				Version:    s[2],
			}

			// Handle "replace" in module file if any
			if len(s) > 3 && s[3] == "=>" {
				mod.SourcePath = s[4]

				// Handle replaces with a relative target. For example:
				// "replace github.com/status-im/status-go/protocol => ./protocol"
				if strings.HasPrefix(s[4], ".") || strings.HasPrefix(s[4], "/") {
					mod.Dir, err = filepath.Abs(s[4])
					if err != nil {
						fmt.Printf("invalid relative path: %v", err)
						os.Exit(1)
					}
				} else {
					mod.SourceVersion = s[5]

					dir, err := pkgModPath(mod.SourcePath, mod.SourceVersion)
					if err != nil {
						fmt.Printf("Error! couldn't resolve module path for %q: %v\n", mod.SourcePath, err)
						os.Exit(1)
					}
					mod.Dir = dir
				}
			} else {
				dir, err := pkgModPath(mod.ImportPath, mod.Version)
				if err != nil {
					fmt.Printf("Error! couldn't resolve module path for %q: %v\n", mod.ImportPath, err)
					os.Exit(1)
				}
				mod.Dir = dir
			}

			if _, err := os.Stat(mod.Dir); os.IsNotExist(err) {
				fmt.Printf("Error! %q module path does not exist (importParth=%s), check $GOPATH/pkg/mod\n", mod.Dir, mod.ImportPath)
				os.Exit(1)
			}

			// Build list of files to module path source to project vendor folder
			mod.VendorList = buildModVendorList(copyPat, mod)
			// Append directories we need to also include which may not be in vendor/modules.txt.
			for _, dir := range additionalDirsToInclude {
				if strings.HasPrefix(dir, mod.ImportPath) {
					mod.Pkgs = append(mod.Pkgs, dir)
				}
			}

			modules = append(modules, mod)

			if *fullCopyFlag {
				mod.Pkgs = append(mod.Pkgs, mod.ImportPath)
			}
			continue
		}

		if !(*fullCopyFlag) {
			mod.Pkgs = append(mod.Pkgs, line)
		}
	}

	// Filter out files not part of the mod.Pkgs
	for _, mod := range modules {
		if len(mod.VendorList) == 0 {
			continue
		}
		for vendorFile := range mod.VendorList {
			for _, subpkg := range mod.Pkgs {
				path := filepath.Join(mod.Dir, importPathIntersect(mod.ImportPath, subpkg))

				x := strings.Index(vendorFile, path)
				if x == 0 {
					mod.VendorList[vendorFile] = true
				}
			}
		}
		for vendorFile, toggle := range mod.VendorList {
			if !toggle {
				delete(mod.VendorList, vendorFile)
			}
		}
	}

	// Copy mod vendor list files to ./vendor/
	for _, mod := range modules {
		for vendorFile := range mod.VendorList {
			x := strings.Index(vendorFile, mod.Dir)
			if x < 0 {
				fmt.Println("Error! vendor file doesn't belong to mod, strange.")
				os.Exit(1)
			}

			localPath := fmt.Sprintf("%s%s", mod.ImportPath, vendorFile[len(mod.Dir):])
			localFile := fmt.Sprintf("./vendor/%s", localPath)

			if *verboseFlag {
				fmt.Printf("vendoring %s\n", localPath)
			}

			if err := os.MkdirAll(filepath.Dir(localFile), os.ModePerm); err != nil {
				fmt.Printf("Error! %s - unable to create directory %s\n", err.Error(), filepath.Dir(localFile))
				os.Exit(1)
			}

			var opt copy.Options
			opt.PermissionControl = copy.AddPermission(0644)
			if err := copy.Copy(vendorFile, localFile, opt); err != nil {
				fmt.Printf("Error! %s - unable to copy file %s\n", err.Error(), vendorFile)
				os.Exit(1)
			}
		}
	}
}

func buildModVendorList(copyPat []string, mod *Mod) map[string]bool {
	vendorList := map[string]bool{}

	for _, pat := range copyPat {
		var matches []string
		var err error
		if len(pat) > 0 {
			matches, err = zglob.Glob(filepath.Join(mod.Dir, pat))
		} else {
			matches, err = getDirAllEntryPathsFollowSymlink(mod.Dir, true)
		}
		if err != nil {
			fmt.Println("Error! glob match failure:", err)
			os.Exit(1)
		}

		for _, m := range matches {
			vendorList[m] = false
		}
	}

	return vendorList
}

func importPathIntersect(basePath, pkgPath string) string {
	if strings.Index(pkgPath, basePath) != 0 {
		return ""
	}
	return pkgPath[len(basePath):]
}

func normString(str string) (normStr string) {
	for _, char := range str {
		if unicode.IsUpper(char) {
			normStr += "!" + string(unicode.ToLower(char))
		} else {
			normStr += string(char)
		}
	}
	return
}

func pkgModPath(importPath, version string) (string, error) {
	normPath := normString(importPath)
	normVersion := normString(version)
	return filepath.Join(build.Default.GOPATH, "pkg", "mod", fmt.Sprintf("%s@%s", normPath, normVersion)), nil
}

func copyFile(src, dst string) (int64, error) {
	srcStat, err := os.Stat(src)
	if err != nil {
		return 0, err
	}

	if !srcStat.Mode().IsRegular() {
		return 0, fmt.Errorf("%s is not a regular file", src)
	}

	srcFile, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = srcFile.Close()
	}()

	dstFile, err := os.Create(dst)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = dstFile.Close()
	}()

	return io.Copy(dstFile, srcFile)
}

// getDirAllEntryPathsFollowSymlink gets all the file or dir paths in the specified directory recursively.
func getDirAllEntryPathsFollowSymlink(dirname string, incl bool) ([]string, error) {
	// Remove the trailing path separator if dirname has.
	dirname = strings.TrimSuffix(dirname, string(os.PathSeparator))

	infos, err := os.ReadDir(dirname)
	if err != nil {
		return nil, err
	}

	paths := make([]string, 0, len(infos))
	// Include current dir.
	if incl {
		paths = append(paths, dirname)
	}

	for _, info := range infos {
		path := dirname + string(os.PathSeparator) + info.Name()
		realInfo, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if realInfo.IsDir() {
			tmp, err := getDirAllEntryPathsFollowSymlink(path, incl)
			if err != nil {
				return nil, err
			}
			paths = append(paths, tmp...)
			continue
		}
		paths = append(paths, path)
	}
	return paths, nil
}
