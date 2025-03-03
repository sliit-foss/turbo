package hashing

import (
	"bufio"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pkg/errors"
	gitignore "github.com/sabhiram/go-gitignore"
	"github.com/vercel/turbo/cli/internal/doublestar"
	"github.com/vercel/turbo/cli/internal/encoding/gitoutput"
	"github.com/vercel/turbo/cli/internal/fs"
	"github.com/vercel/turbo/cli/internal/globby"
	"github.com/vercel/turbo/cli/internal/turbopath"
	"github.com/vercel/turbo/cli/internal/util"
)

// PackageDepsOptions are parameters for getting git hashes for a filesystem
type PackageDepsOptions struct {
	// PackagePath is the folder path to derive the package dependencies from. This is typically the folder
	// containing package.json. If omitted, the default value is the current working directory.
	PackagePath turbopath.AnchoredSystemPath

	InputPatterns []string
}

func safeCompileIgnoreFile(filepath turbopath.AbsoluteSystemPath) (*gitignore.GitIgnore, error) {
	if filepath.FileExists() {
		return gitignore.CompileIgnoreFile(filepath.ToString())
	}
	// no op
	return gitignore.CompileIgnoreLines([]string{}...), nil
}

func getPackageFileHashesFromProcessingGitIgnore(rootPath turbopath.AbsoluteSystemPath, packagePath turbopath.AnchoredSystemPath, inputs []string) (map[turbopath.AnchoredUnixPath]string, error) {
	result := make(map[turbopath.AnchoredUnixPath]string)
	absolutePackagePath := packagePath.RestoreAnchor(rootPath)

	// Instead of implementing all gitignore properly, we hack it. We only respect .gitignore in the root and in
	// the directory of a package.
	ignore, err := safeCompileIgnoreFile(rootPath.UntypedJoin(".gitignore"))
	if err != nil {
		return nil, err
	}

	ignorePkg, err := safeCompileIgnoreFile(absolutePackagePath.UntypedJoin(".gitignore"))
	if err != nil {
		return nil, err
	}

	includePattern := ""
	excludePattern := ""
	if len(inputs) > 0 {
		var includePatterns []string
		var excludePatterns []string
		for _, pattern := range inputs {
			if len(pattern) > 0 && pattern[0] == '!' {
				excludePatterns = append(excludePatterns, absolutePackagePath.UntypedJoin(pattern[1:]).ToString())
			} else {
				includePatterns = append(includePatterns, absolutePackagePath.UntypedJoin(pattern).ToString())
			}
		}
		if len(includePatterns) > 0 {
			includePattern = "{" + strings.Join(includePatterns, ",") + "}"
		}
		if len(excludePatterns) > 0 {
			excludePattern = "{" + strings.Join(excludePatterns, ",") + "}"
		}
	}

	err = fs.Walk(absolutePackagePath.ToStringDuringMigration(), func(name string, isDir bool) error {
		convertedName := turbopath.AbsoluteSystemPathFromUpstream(name)
		rootMatch := ignore.MatchesPath(convertedName.ToString())
		otherMatch := ignorePkg.MatchesPath(convertedName.ToString())
		if !rootMatch && !otherMatch {
			if !isDir {
				if includePattern != "" {
					val, err := doublestar.PathMatch(includePattern, convertedName.ToString())
					if err != nil {
						return err
					}
					if !val {
						return nil
					}
				}
				if excludePattern != "" {
					val, err := doublestar.PathMatch(excludePattern, convertedName.ToString())
					if err != nil {
						return err
					}
					if val {
						return nil
					}
				}
				hash, err := fs.GitLikeHashFile(convertedName)
				if err != nil {
					return fmt.Errorf("could not hash file %v. \n%w", convertedName.ToString(), err)
				}

				relativePath, err := convertedName.RelativeTo(absolutePackagePath)
				if err != nil {
					return fmt.Errorf("File path cannot be made relative: %w", err)
				}
				result[relativePath.ToUnixPath()] = hash
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func getPackageFileHashesFromInputs(rootPath turbopath.AbsoluteSystemPath, packagePath turbopath.AnchoredSystemPath, inputs []string) (map[turbopath.AnchoredUnixPath]string, error) {
	absolutePackagePath := packagePath.RestoreAnchor(rootPath)
	// Add all the checked in hashes.

	// make a copy of the inputPatterns array, because we may be appending to it later.
	calculatedInputs := make([]string, len(inputs))
	copy(calculatedInputs, inputs)

	// Add in package.json and turbo.json to input patterns. Both file paths are relative to pkgPath
	//
	// - package.json is an input because if the `scripts` in
	// 		the package.json change (i.e. the tasks that turbo executes), we want
	// 		a cache miss, since any existing cache could be invalid.
	// - turbo.json because it's the definition of the tasks themselves. The root turbo.json
	// 		is similarly included in the global hash. This file may not exist in the workspace, but
	// 		that is ok, because it will get ignored downstream.
	calculatedInputs = append(calculatedInputs, "package.json")
	calculatedInputs = append(calculatedInputs, "turbo.json")

	// The input patterns are relative to the package.
	// However, we need to change the globbing to be relative to the repo root.
	// Prepend the package path to each of the input patterns.
	prefixedInputPatterns := []string{}
	prefixedExcludePatterns := []string{}
	for _, pattern := range calculatedInputs {
		if len(pattern) > 0 && pattern[0] == '!' {
			rerooted, err := rootPath.PathTo(absolutePackagePath.UntypedJoin(pattern[1:]))
			if err != nil {
				return nil, err
			}
			prefixedExcludePatterns = append(prefixedExcludePatterns, rerooted)
		} else {
			rerooted, err := rootPath.PathTo(absolutePackagePath.UntypedJoin(pattern))
			if err != nil {
				return nil, err
			}
			prefixedInputPatterns = append(prefixedInputPatterns, rerooted)
		}
	}
	absoluteFilesToHash, err := globby.GlobFiles(rootPath.ToStringDuringMigration(), prefixedInputPatterns, prefixedExcludePatterns)

	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve input globs %v", calculatedInputs)
	}

	filesToHash := make([]turbopath.AnchoredSystemPath, len(absoluteFilesToHash))
	for i, rawPath := range absoluteFilesToHash {
		relativePathString, err := absolutePackagePath.RelativePathString(rawPath)

		if err != nil {
			return nil, errors.Wrapf(err, "not relative to package: %v", rawPath)
		}

		filesToHash[i] = turbopath.AnchoredSystemPathFromUpstream(relativePathString)
	}

	// Note that in this scenario, we don't need to check git status.
	// We're hashing the current state, not state at a commit.
	result, err := GetHashesForFiles(absolutePackagePath, filesToHash)
	if err != nil {
		return nil, errors.Wrap(err, "failed hashing resolved inputs globs")
	}

	return result, nil
}

// GetPackageFileHashes Builds an object containing git hashes for the files under the specified `packagePath` folder.
func GetPackageFileHashes(rootPath turbopath.AbsoluteSystemPath, packagePath turbopath.AnchoredSystemPath, inputs []string) (map[turbopath.AnchoredUnixPath]string, error) {
	if len(inputs) == 0 {
		result, err := getPackageFileHashesFromGitIndex(rootPath, packagePath)
		if err != nil {
			return getPackageFileHashesFromProcessingGitIgnore(rootPath, packagePath, nil)
		}
		return result, nil
	}

	result, err := getPackageFileHashesFromInputs(rootPath, packagePath, inputs)
	if err != nil {
		return getPackageFileHashesFromProcessingGitIgnore(rootPath, packagePath, inputs)
	}
	return result, nil
}

// GetHashesForFiles hashes the list of given files, then returns a map of normalized path to hash.
// This map is suitable for cross-platform caching.
func GetHashesForFiles(rootPath turbopath.AbsoluteSystemPath, files []turbopath.AnchoredSystemPath) (map[turbopath.AnchoredUnixPath]string, error) {
	// Try to use `git` first.
	gitHashedFiles, err := gitHashObject(rootPath, files)
	if err == nil {
		return gitHashedFiles, nil
	}

	// Fall back to manual hashing.
	return manuallyHashFiles(rootPath, files)
}

// gitHashObject returns a map of paths to their SHA hashes calculated by passing the paths to `git hash-object`.
// `git hash-object` expects paths to use Unix separators, even on Windows.
//
// Note: paths of files to hash passed to `git hash-object` are processed as relative to the given anchor.
// For that reason we convert all input paths and make them relative to the anchor prior to passing them
// to `git hash-object`.
func gitHashObject(anchor turbopath.AbsoluteSystemPath, filesToHash []turbopath.AnchoredSystemPath) (map[turbopath.AnchoredUnixPath]string, error) {
	fileCount := len(filesToHash)
	output := make(map[turbopath.AnchoredUnixPath]string, fileCount)

	if fileCount > 0 {
		cmd := exec.Command(
			"git",           // Using `git` from $PATH,
			"hash-object",   // hash a file,
			"--stdin-paths", // using a list of newline-separated paths from stdin.
		)
		cmd.Dir = anchor.ToString() // Start at this directory.

		// The functionality for gitHashObject is different enough that it isn't reasonable to
		// generalize the behavior for `runGitCmd`. In fact, it doesn't even use the `gitoutput`
		// encoding library, instead relying on its own separate `bufio.Scanner`.

		// We're going to send the list of files in via `stdin`, so we grab that pipe.
		// This prevents a huge number of encoding issues and shell compatibility issues
		// before they even start.
		stdinPipe, stdinPipeError := cmd.StdinPipe()
		if stdinPipeError != nil {
			return nil, stdinPipeError
		}

		// Kick the processing off in a goroutine so while that is doing its thing we can go ahead
		// and wire up the consumer of `stdout`.
		go func() {
			defer util.CloseAndIgnoreError(stdinPipe)

			// `git hash-object` understands all relative paths to be relative to the repository.
			// This function's result needs to be relative to `rootPath`.
			// We convert all files to absolute paths and assume that they will be inside of the repository.
			for _, file := range filesToHash {
				converted := file.RestoreAnchor(anchor)

				// `git hash-object` expects paths to use Unix separators, even on Windows.
				// `git hash-object` expects paths to be one per line so we must escape newlines.
				// In order to understand the escapes, the path must be quoted.
				// In order to quote the path, the quotes in the path must be escaped.
				// Other than that, we just write everything with full Unicode.
				stringPath := converted.ToString()
				toSlashed := filepath.ToSlash(stringPath)
				escapedNewLines := strings.ReplaceAll(toSlashed, "\n", "\\n")
				escapedQuotes := strings.ReplaceAll(escapedNewLines, "\"", "\\\"")
				prepared := fmt.Sprintf("\"%s\"\n", escapedQuotes)
				_, err := io.WriteString(stdinPipe, prepared)
				if err != nil {
					return
				}
			}
		}()

		// This gives us an io.ReadCloser so that we never have to read the entire input in
		// at a single time. It is doing stream processing instead of string processing.
		stdoutPipe, stdoutPipeError := cmd.StdoutPipe()
		if stdoutPipeError != nil {
			return nil, fmt.Errorf("failed to read `git hash-object`: %w", stdoutPipeError)
		}

		startError := cmd.Start()
		if startError != nil {
			return nil, fmt.Errorf("failed to read `git hash-object`: %w", startError)
		}

		// The output of `git hash-object` is a 40-character SHA per input, then a newline.
		// We need to track the SHA that corresponds to the input file path.
		index := 0
		hashes := make([]string, len(filesToHash))
		scanner := bufio.NewScanner(stdoutPipe)

		// Read the output line-by-line (which is our separator) until exhausted.
		for scanner.Scan() {
			bytes := scanner.Bytes()

			scanError := scanner.Err()
			if scanError != nil {
				return nil, fmt.Errorf("failed to read `git hash-object`: %w", scanError)
			}

			hashError := gitoutput.CheckObjectName(bytes)
			if hashError != nil {
				return nil, fmt.Errorf("failed to read `git hash-object`: %s", "invalid hash received")
			}

			// Worked, save it off.
			hashes[index] = string(bytes)
			index++
		}

		// Waits until stdout is closed before proceeding.
		waitErr := cmd.Wait()
		if waitErr != nil {
			return nil, fmt.Errorf("failed to read `git hash-object`: %w", waitErr)
		}

		// Make sure we end up with a matching number of files and hashes.
		hashCount := len(hashes)
		if fileCount != hashCount {
			return nil, fmt.Errorf("failed to read `git hash-object`: %d files %d hashes", fileCount, hashCount)
		}

		// The API of this method specifies that we return a `map[turbopath.AnchoredUnixPath]string`.
		for i, hash := range hashes {
			filePath := filesToHash[i]
			output[filePath.ToUnixPath()] = hash
		}
	}

	return output, nil
}

func manuallyHashFiles(rootPath turbopath.AbsoluteSystemPath, files []turbopath.AnchoredSystemPath) (map[turbopath.AnchoredUnixPath]string, error) {
	hashObject := make(map[turbopath.AnchoredUnixPath]string)
	for _, file := range files {
		hash, err := fs.GitLikeHashFile(file.RestoreAnchor(rootPath))
		if err != nil {
			return nil, fmt.Errorf("could not hash file %v. \n%w", file.ToString(), err)
		}

		hashObject[file.ToUnixPath()] = hash
	}
	return hashObject, nil
}

// runGitCommand provides boilerplate command handling for `ls-tree`, `ls-files`, and `status`
// Rather than doing string processing, it does stream processing of `stdout`.
func runGitCommand(cmd *exec.Cmd, commandName string, handler func(io.Reader) *gitoutput.Reader) ([][]string, error) {
	stdoutPipe, pipeError := cmd.StdoutPipe()
	if pipeError != nil {
		return nil, fmt.Errorf("failed to read `git %s`: %w", commandName, pipeError)
	}

	startError := cmd.Start()
	if startError != nil {
		return nil, fmt.Errorf("failed to read `git %s`: %w", commandName, startError)
	}

	reader := handler(stdoutPipe)
	entries, readErr := reader.ReadAll()
	if readErr != nil {
		return nil, fmt.Errorf("failed to read `git %s`: %w", commandName, readErr)
	}

	waitErr := cmd.Wait()
	if waitErr != nil {
		return nil, fmt.Errorf("failed to read `git %s`: %w", commandName, waitErr)
	}

	return entries, nil
}

// gitLsTree returns a map of paths to their SHA hashes starting at a particular directory
// that are present in the `git` index at a particular revision.
func gitLsTree(rootPath turbopath.AbsoluteSystemPath) (map[turbopath.AnchoredUnixPath]string, error) {
	cmd := exec.Command(
		"git",     // Using `git` from $PATH,
		"ls-tree", // list the contents of the git index,
		"-r",      // recursively,
		"-z",      // with each file path relative to the invocation directory and \000-terminated,
		"HEAD",    // at this specified version.
	)
	cmd.Dir = rootPath.ToString() // Include files only from this directory.

	entries, err := runGitCommand(cmd, "ls-tree", gitoutput.NewLSTreeReader)
	if err != nil {
		return nil, err
	}

	output := make(map[turbopath.AnchoredUnixPath]string, len(entries))

	for _, entry := range entries {
		lsTreeEntry := gitoutput.LsTreeEntry(entry)
		output[turbopath.AnchoredUnixPathFromUpstream(lsTreeEntry.GetField(gitoutput.Path))] = lsTreeEntry[2]
	}

	return output, nil
}

// getTraversePath gets the distance of the current working directory to the repository root.
// This is used to convert repo-relative paths to cwd-relative paths.
//
// `git rev-parse --show-cdup` always returns Unix paths, even on Windows.
func getTraversePath(rootPath turbopath.AbsoluteSystemPath) (turbopath.RelativeUnixPath, error) {
	cmd := exec.Command("git", "rev-parse", "--show-cdup")
	cmd.Dir = rootPath.ToString()

	traversePath, err := cmd.Output()
	if err != nil {
		return "", err
	}

	trimmedTraversePath := strings.TrimSuffix(string(traversePath), "\n")

	return turbopath.RelativeUnixPathFromUpstream(trimmedTraversePath), nil
}

// Don't shell out if we already know where you are in the repository.
// `memoize` is a good candidate for generics.
func memoizeGetTraversePath() func(turbopath.AbsoluteSystemPath) (turbopath.RelativeUnixPath, error) {
	cacheMutex := &sync.RWMutex{}
	cachedResult := map[turbopath.AbsoluteSystemPath]turbopath.RelativeUnixPath{}
	cachedError := map[turbopath.AbsoluteSystemPath]error{}

	return func(rootPath turbopath.AbsoluteSystemPath) (turbopath.RelativeUnixPath, error) {
		cacheMutex.RLock()
		result, resultExists := cachedResult[rootPath]
		err, errExists := cachedError[rootPath]
		cacheMutex.RUnlock()

		if resultExists && errExists {
			return result, err
		}

		invokedResult, invokedErr := getTraversePath(rootPath)
		cacheMutex.Lock()
		cachedResult[rootPath] = invokedResult
		cachedError[rootPath] = invokedErr
		cacheMutex.Unlock()

		return invokedResult, invokedErr
	}
}

var memoizedGetTraversePath = memoizeGetTraversePath()

// statusCode represents the two-letter status code from `git status` with two "named" fields, x & y.
// They have different meanings based upon the actual state of the working tree. Using x & y maps
// to upstream behavior.
type statusCode struct {
	x string
	y string
}

func (s statusCode) isDelete() bool {
	return s.x == "D" || s.y == "D"
}
