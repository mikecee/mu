package common

import (
	"errors"
	"fmt"
	"github.com/speedata/gogit"
	"github.com/tcnksm/go-gitconfig"
	"os"
	"path"
	"regexp"
)

func findGitRevision(file string) (string, error) {
	gitDir, err := findGitDirectory(file)
	if err != nil {
		return "", err
	}
	log.Debugf("Loading revision from git directory '%s'", gitDir)

	repository, err := gogit.OpenRepository(gitDir)
	if err != nil {
		return "", err
	}
	ref, err := repository.LookupReference("HEAD")
	if err != nil {
		return "", err
	}
	ci, err := repository.LookupCommit(ref.Oid)
	if err != nil {
		return "", err
	}
	return string(ci.Id().String()[:7]), nil
}
func findGitSlug() (string, error) {
	url, err := gitconfig.OriginURL()
	if err != nil {
		return "", err
	}

	httpRegex := regexp.MustCompile("^http(s?)://.*github.com.*/(.+)/(.+).git$")
	sshRegex := regexp.MustCompile("github.com:(.+)/(.+).git$")
	if matches := httpRegex.FindStringSubmatch(url); matches != nil {
		return fmt.Sprintf("%s/%s", matches[2], matches[3]), nil
	} else if matches := sshRegex.FindStringSubmatch(url); matches != nil {
		return fmt.Sprintf("%s/%s", matches[1], matches[2]), nil
	}
	return url, nil
}

func findGitDirectory(fromFile string) (string, error) {
	log.Debugf("Searching for git directory in %s", fromFile)
	fi, err := os.Stat(fromFile)
	if err != nil {
		return "", err
	}

	var dir string
	if fi.Mode().IsDir() {
		dir = fromFile
	} else {
		dir = path.Dir(fromFile)
	}

	gitPath := path.Join(dir, ".git")
	fi, err = os.Stat(gitPath)
	if err == nil && fi.Mode().IsDir() {
		return gitPath, nil
	} else if dir == "/" {
		return "", errors.New("Unable to find git repo")
	} else {
		return findGitDirectory(path.Dir(dir))
	}

}
