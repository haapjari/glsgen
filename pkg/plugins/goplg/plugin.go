package goplg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/haapjari/glsgen/pkg/models"
	"github.com/haapjari/glsgen/pkg/utils"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

var (
	GITHUB_API_TOKEN                string = fmt.Sprintf("%v", utils.GetGithubToken())
	GITHUB_USERNAME                 string = fmt.Sprintf("%v", utils.GetGithubUsername())
	SOURCEGRAPH_GRAPHQL_API_BASEURL string = utils.GetSourceGraphGQLApi()
	GITHUB_GRAPHQL_API_BASEURL      string = utils.GetGitHubGQLApi()
)

// Plugin is tested with Go Version 1.19.4. Last Update: 15.1.2023.
type GoPlugin struct {
	GitHubApiToken string
	GitHubUsername string
	HttpClient     *http.Client
	Parser         *Parser
	DatabaseClient *gorm.DB
	GitHubClient   *http.Client
	MaxRoutines    int
	LibraryCache   map[string]int
	GoMods         map[string]*GoMod
	DependencyMap  map[string][]string
}

func NewGoPlugin(DatabaseClient *gorm.DB) *GoPlugin {
	g := new(GoPlugin)
	var err error

	g.HttpClient = &http.Client{Timeout: time.Minute * 10}
	g.GitHubClient = oauth2.NewClient(context.Background(), oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: GITHUB_API_TOKEN},
	))
	g.DatabaseClient = DatabaseClient
	g.LibraryCache = make(map[string]int)
	g.Parser = NewParser()
	g.GoMods = make(map[string]*GoMod)
	g.MaxRoutines, err = strconv.Atoi(utils.GetMaxGoRoutines())
	g.DependencyMap = make(map[string][]string)
	utils.CheckErr(err)

	return g
}

// Entrypoint for the Handler.
func (g *GoPlugin) GenerateRepositoryData(c int) []models.Repository {
	unprocessedRepositories := g.fetchRepositories(c)
	repositoriesWithoutLibrarySize := g.processRepositories(unprocessedRepositories)
	processedRepositories := g.processLibraries(repositoriesWithoutLibrarySize)

	return processedRepositories
	// TODO
	// g.fixBrokenEntries()
	// g.pruneDuplicates()
}

// Queries SourceGraph and GitHub GraphQL API's, and saves the metadata from the queries
// to database table "repositories".
func (g *GoPlugin) fetchRepositories(count int) []models.Repository {
	log.Println("Fetching repositories...")

	var newRepositories []models.Repository

	queryString := `{
		search(query: "lang:go + AND select:repo AND repohasfile:go.mod AND count:` + strconv.Itoa(count) + `", version:V2) { results {
				repositories {
					name
				}
			}
		}
	}`

	rawRequest := map[string]string{
		"query": queryString,
	}

	requestBody, err := json.Marshal(rawRequest)
	utils.CheckErr(err)

	bytesBody := bytes.NewBuffer(requestBody)

	httpRequest, err := http.NewRequest("POST", SOURCEGRAPH_GRAPHQL_API_BASEURL, bytesBody)
	httpRequest.Header.Set("Content-Type", "application/json")
	utils.CheckErr(err)

	httpResponse, err := g.HttpClient.Do(httpRequest)
	utils.CheckErr(err)

	defer httpResponse.Body.Close()

	httpResponseBody, err := ioutil.ReadAll(httpResponse.Body)
	utils.CheckErr(err)

	var sourceGraphResponseStruct models.SourceGraphResponseStruct
	json.Unmarshal([]byte(httpResponseBody), &sourceGraphResponseStruct)

	sourceGraphResponseLength := len(sourceGraphResponseStruct.Data.Search.Results.Repositories)
	repositories := sourceGraphResponseStruct.Data.Search.Results.Repositories

	var waitGroup sync.WaitGroup
	semaphore := make(chan int, g.MaxRoutines)
	var mu sync.Mutex

	parser := NewParser()

	for i := 0; i < sourceGraphResponseLength; i++ {
		waitGroup.Add(1)
		semaphore <- 1
		go func(i int) {
			_, projectName := parser.ParseRepository(repositories[i].Name)
			repository := models.Repository{RepositoryName: projectName, RepositoryUrl: repositories[i].Name, OpenIssueCount: "", ClosedIssueCount: "", OriginalCodebaseSize: "", LibraryCodebaseSize: "", RepositoryType: "", PrimaryLanguage: ""}
			if !g.repositoryExists(repository) {
				log.Println("Database entry from: " + repository.RepositoryUrl)
				g.DatabaseClient.Create(&repository)
				mu.Lock()
				newRepositories = append(newRepositories, repository)
				mu.Unlock()
			}
			defer func() {
				<-semaphore
				waitGroup.Done()
			}()
		}(i)
	}

	waitGroup.Wait()

	for !(len(semaphore) == 0) {
		continue
	}

	// Reads the repositories -tables values to memory, crafts a GitHub GraphQL requests of the
	// repositories, and appends the database entries with Open Issue Count, Closed Issue Count,
	// Commit Count, Original Codebase Size, Repository Type, Primary Language, Stargazers Count,
	// Creation Date, License.
	// TODO: We could stress the PostgreSQL a less, if we woouldn't do the inputting to the database before.
	for i := 0; i < len(newRepositories); i++ {
		waitGroup.Add(1)
		semaphore <- 1
		go func(i int) {
			if !g.hasBeenEnriched(newRepositories[i]) {
				repositoryOwner, repositoryName := g.Parser.ParseRepository(newRepositories[i].RepositoryUrl)
				queryStr := fmt.Sprintf(`{
					repository(owner: "%s", name: "%s") {
						defaultBranchRef {
							target {
								... on Commit {
								history {
									totalCount
								}
							}
						}
					}	
					openIssues: issues(states:OPEN) {
						totalCount
					}
					closedIssues: issues(states:CLOSED) {
						totalCount
					}
					languages {
						totalSize
					}
					stargazerCount
					licenseInfo {
						key
					}
					createdAt
					latestRelease{
						publishedAt
					}
					primaryLanguage{
						name
					}
				}
			}`, repositoryOwner, repositoryName)

				rawRequestBody := map[string]string{
					"query": queryStr,
				}

				requestBody, err := json.Marshal(rawRequestBody)
				utils.CheckErr(err)

				httpRequest, err := http.NewRequest("POST", GITHUB_GRAPHQL_API_BASEURL, bytes.NewBuffer(requestBody))
				httpRequest.Header.Set("Accept", "application/vnd.github.v3+json")
				utils.CheckErr(err)

				httpResponse, err := g.GitHubClient.Do(httpRequest)
				utils.CheckErr(err)

				defer httpResponse.Body.Close()

				httpResponseBody, err := ioutil.ReadAll(httpResponse.Body)
				utils.CheckErr(err)

				var gitHubResponseStruct models.GitHubResponseStruct
				json.Unmarshal([]byte(httpResponseBody), &gitHubResponseStruct)

				var currentRepository models.Repository

				if err := g.DatabaseClient.Where("repository_url = ?", newRepositories[i].RepositoryUrl).First(&currentRepository).Error; err != nil {
					utils.CheckErr(err)
				}

				currentRepository.RepositoryName = repositoryName
				currentRepository.RepositoryUrl = newRepositories[i].RepositoryUrl
				currentRepository.OpenIssueCount = strconv.Itoa(gitHubResponseStruct.Data.Repository.OpenIssues.TotalCount)
				currentRepository.ClosedIssueCount = strconv.Itoa(gitHubResponseStruct.Data.Repository.ClosedIssues.TotalCount)
				currentRepository.CommitCount = strconv.Itoa(gitHubResponseStruct.Data.Repository.DefaultBranchRef.Target.History.TotalCount)
				currentRepository.RepositoryType = "primary"
				currentRepository.PrimaryLanguage = gitHubResponseStruct.Data.Repository.PrimaryLanguage.Name
				currentRepository.CreationDate = gitHubResponseStruct.Data.Repository.CreatedAt
				currentRepository.StargazerCount = strconv.Itoa(gitHubResponseStruct.Data.Repository.StargazerCount)
				currentRepository.LicenseInfo = gitHubResponseStruct.Data.Repository.LicenseInfo.Key
				currentRepository.LatestRelease = gitHubResponseStruct.Data.Repository.LatestRelease.PublishedAt

				g.DatabaseClient.Model(&currentRepository).Updates(currentRepository)
			}
			defer func() {
				<-semaphore
				waitGroup.Done()
			}()
		}(i)
	}
	waitGroup.Wait()

	for !(len(semaphore) == 0) {
		continue
	}

	return newRepositories
}

// Processes entities from the database table "repositories", and calculates the amount
// of code in the project.
func (g *GoPlugin) processRepositories(unprocessedRepositories []models.Repository) []models.Repository {
	var waitGroup sync.WaitGroup
	semaphore := make(chan int, g.MaxRoutines)
	var goModsMutex sync.Mutex

	// Create "tmp" directory, if the directory doesn't already exists.
	if _, err := os.Stat("tmp"); os.IsNotExist(err) {
		if err := os.Mkdir("tmp", 0777); err != nil {
			utils.CheckErr(err)
		}
	}

	// Append the https:// and .git prefix and postfix the RepositoryUrl variables.
	for i := 0; i < len(unprocessedRepositories); i++ {
		var databaseEntry models.Repository
		g.DatabaseClient.Table("repositories").Where("repository_url = ?", unprocessedRepositories[i].RepositoryUrl).First(&databaseEntry)
		// If the field "OriginalCodebaseSize" is empty, but it has a name, that means
		// it exists in the database, but is not analyzed yet -> proceed.
		if databaseEntry.OriginalCodebaseSize == "" && databaseEntry.RepositoryName != "" {
			waitGroup.Add(1)
			semaphore <- 1
			go func(i int) {
				log.Println("Processing repository: " + unprocessedRepositories[i].RepositoryName)
				repositoryUrl := "https://" + unprocessedRepositories[i].RepositoryUrl + ".git"

				err := utils.Command("git", "clone", "--depth", "1", repositoryUrl, "tmp"+"/"+unprocessedRepositories[i].RepositoryName)
				if err != nil {
					fmt.Printf("error while cloning repository %s: %s, skipping...\n", repositoryUrl, err)
				}

				repositoryCodeLines, err := g.calculateCodeLines("tmp" + "/" + unprocessedRepositories[i].RepositoryName)
				if err != nil {
					fmt.Print(err.Error())
				}

				goModsMutex.Lock()
				goMod, err := parseGoMod(unprocessedRepositories[i].RepositoryName + "/" + "go.mod")
				if err != nil {
					fmt.Printf("error, while parsing the modfile of "+unprocessedRepositories[i].RepositoryName+": %s", err)
				}
				g.GoMods[unprocessedRepositories[i].RepositoryUrl] = goMod
				goModsMutex.Unlock()

				g.generateDependenciesMap(unprocessedRepositories[i])

				var repositoryStruct models.Repository
				if err := g.DatabaseClient.Where("repository_url = ?", unprocessedRepositories[i].RepositoryUrl).First(&repositoryStruct).Error; err != nil {
					utils.CheckErr(err)
				}

				repositoryStruct.OriginalCodebaseSize = strconv.Itoa(repositoryCodeLines)
				unprocessedRepositories[i] = repositoryStruct

				g.DatabaseClient.Model(&repositoryStruct).Updates(repositoryStruct)

				defer func() {
					waitGroup.Done()
					<-semaphore
				}()
			}(i)
		}
		waitGroup.Wait()
		g.pruneTemporaryFolder()

		for !(len(semaphore) == 0) {
			continue
		}
	}

	return unprocessedRepositories
}

// Loop through repositories, generate the dependency map from the go.mod files of the
// repositories, download the dependencies to the local disk, calculate their sizes and
// save the values to the database.
// TODO: Goroutines
func (g *GoPlugin) processLibraries(repositoriesWithoutLibrarySize []models.Repository) []models.Repository {
	libraries := g.DependencyMap

	utils.CopyFile("go.mod", "go.mod.bak")
	utils.CopyFile("go.sum", "go.sum.bak")
	os.Setenv("GOPATH", utils.GetProcessDirPath())

	for i := 0; i < len(repositoriesWithoutLibrarySize); i++ {
		repositoryName := repositoriesWithoutLibrarySize[i].RepositoryName
		repositoryUrl := repositoriesWithoutLibrarySize[i].RepositoryUrl
		totalLibraryCodeLines := 0
		libraryCodebaseSize := repositoriesWithoutLibrarySize[i].LibraryCodebaseSize

		if libraryCodebaseSize == "" {
			log.Println(repositoriesWithoutLibrarySize[i].RepositoryName + " processing " + strconv.Itoa(len(libraries[repositoryName])) + " libraries...")
			// Loop through the libraries, which are saved to the map, where dependencies
			// are accessible by repository name. Download them to the local disk, calculate
			// their sizes and append to the 'l' -variable.
			for j := 0; j < len(libraries[repositoryName]); j++ {
				librariesLeft := len(libraries[repositoryName]) - j
				log.Println(repositoriesWithoutLibrarySize[i].RepositoryName + " has " + strconv.Itoa(librariesLeft) + " libraries to process...")
				value, ok := g.LibraryCache[libraries[repositoryName][j]]
				if ok {
					totalLibraryCodeLines += value
				} else {
					// Syncronising these parts with a channel.
					downloadableFormatUrl := downloadableFormat(libraries[repositoryName][j])
					err := utils.Command("go", "get", "-d", "-v", downloadableFormatUrl)
					if err != nil {
						fmt.Printf("error while processing library %s: %s, skipping...\n", libraries[repositoryName][j], err)
					}

					libraryPath := utils.GetProcessDirPath() + "/" + "pkg/mod" + "/" + parseLibraryUrl(libraries[repositoryName][j])
					libraryCodeLines, err := g.calculateCodeLines(libraryPath)
					if err != nil {
						fmt.Println("error, while calculating library code lines:", err.Error())
					}
					g.LibraryCache[libraries[repositoryName][j]] = libraryCodeLines
					totalLibraryCodeLines += libraryCodeLines

				}
			}

			g.pruneTemporaryFolder()

			utils.RemoveFiles("go.mod", "go.sum")
			utils.CopyFile("go.mod.bak", "go.mod")
			utils.CopyFile("go.sum.bak", "go.sum")

			var repositoryStruct models.Repository
			if err := g.DatabaseClient.Where("repository_url = ?", repositoryUrl).First(&repositoryStruct).Error; err != nil {
				utils.CheckErr(err)
			}

			repositoryStruct.LibraryCodebaseSize = strconv.Itoa(totalLibraryCodeLines)
			g.DatabaseClient.Model(&repositoryStruct).Updates(repositoryStruct)

			repositoriesWithoutLibrarySize[i] = repositoryStruct
		}
	}

	os.Setenv("GOPATH", utils.GetGoPath())

	utils.RemoveFiles("go.mod", "go.sum")
	utils.CopyFile("go.mod.bak", "go.mod")
	utils.CopyFile("go.sum.bak", "go.sum")
	utils.RemoveFiles("go.mod.bak", "go.sum.bak")

	return repositoriesWithoutLibrarySize
}

// Prunes the duplicate entries from the repository.
// TODO: Refactor this to delete only single duplicate repository. Now it's pretty heavy, since it goes through all the repositories twice.
func (g *GoPlugin) pruneDuplicates() {
	repositories := g.getAllRepositories()
	duplicateRepositories := g.findDuplicates(repositories)

	for i := 0; i < len(duplicateRepositories); i++ {
		var repository models.Repository

		if err := g.DatabaseClient.Where("repository_url = ?", duplicateRepositories[i].RepositoryUrl).First(&repository).Error; err != nil {
			utils.CheckErr(err)
		}
		g.DatabaseClient.Delete(&repository)
	}
}

// Gets a list of repositories and returns a map of repository names and their dependencies,
// which are parsed from the projects "go.mod" -file.
func (g *GoPlugin) generateDependenciesMap(repository models.Repository) {
	repositoryName := repository.RepositoryName
	repositoryUrl := repository.RepositoryUrl

	g.DependencyMap[repositoryName] = append(g.DependencyMap[repositoryName], g.GoMods[repositoryUrl].Require...)

	if g.GoMods[repositoryUrl].Replace != nil {
		replacePaths := g.GoMods[repositoryUrl].Replace
		for i := 0; i < len(replacePaths); i++ {
			if isLocalPath(replacePaths[i]) {
				innerModFilePath := utils.GetProcessDirPath() + "/" + "pkg/mod" + "/" + repositoryUrl + trimFirstRune(replacePaths[i]) + "/" + "go.mod"
				innerModuleFile, err := parseGoMod(innerModFilePath)
				if err != nil {
					fmt.Printf("error, while parsing the inner module file: %s", err)
				} else {
					g.DependencyMap[repositoryName] = append(g.DependencyMap[repositoryName], innerModuleFile.Require...)
				}
			}
		}
	}
	g.DependencyMap[repositoryName] = utils.RemoveDuplicates(g.DependencyMap[repositoryName])
}
