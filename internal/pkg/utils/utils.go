package utils

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"time"
)

// Checks, whether the struct has empty fields.
func CheckEmptyFields(s interface{}) bool {
	var isEmpty = false
	val := reflect.ValueOf(s).Elem()
	for i := 0; i < val.NumField(); i++ {
		valueField := val.Field(i)
		if valueField.Kind() == reflect.String {
			if valueField.String() == "" {
				isEmpty = true
				break
			}
		}
	}
	return isEmpty
}

// removeDuplicates removes duplicates from a slice of strings
func RemoveDuplicates(slice []string) []string {
	// Create a map to keep track of the elements that have already been seen
	seen := make(map[string]struct{}, len(slice))

	// Initialize the result slice
	var result []string

	// Iterate over the slice
	for _, elem := range slice {
		// Check if the element has already been seen
		if _, ok := seen[elem]; !ok {
			// If it has not been seen, add it to the result slice and mark it as seen
			result = append(result, elem)
			seen[elem] = struct{}{}
		}
	}

	return result
}

// Filter empty strings from slice.
func FilterEmpty(slice []string) []string {
	var result []string
	for _, s := range slice {
		if s != "" {
			result = append(result, s)
		}
	}
	return result
}

// Performs a GET request to the specified URL.
func GetRequest(url string) string {
	// Make a GET request to the specified URL
	resp, err := http.Get(url)
	CheckErr(err)

	defer resp.Body.Close()

	// Read the response body into a variable
	body, err := ioutil.ReadAll(resp.Body)
	CheckErr(err)

	return (string(body))
}

func Command(command string, args ...string) error {
	cmd := exec.Command(command, args...)

	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutBuf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)

	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("error: %s", err)
	}

	done := make(chan error)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case <-time.After(120 * time.Second):
		cmd.Process.Kill()
		return errors.New("Command timeout, 120 seconds")
	case err := <-done:
		return err
	}
}

func CheckErr(err error) {
	if err != nil {
		log.Fatalln(err)
	}
}

func RemoveCharFromString(str string, chr rune) string {
	var builder strings.Builder

	for _, ch := range str {
		if ch != chr {
			builder.WriteRune(ch)
		}
	}

	return builder.String()
}

func CopyFile(src string, dst string) error {
	err := Command("cp", src, dst)
	if err != nil {
		fmt.Printf("%s\n", err)
		return err
	}

	return nil
}

func RemoveFiles(pathsToFiles ...string) error {
	args := append([]string{"rm"}, pathsToFiles...)
	err := Command("rm", args...)
	if err != nil {
		fmt.Printf("%s\n", err)
		return err
	}

	return nil
}