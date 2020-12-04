package manager

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-github/v30/github"
	"github.com/micro/go-micro/v2/config"
	log "github.com/micro/go-micro/v2/logger"
	"github.com/micro/go-micro/v2/runtime"
)

var (
	owner   = "micro"
	repo    = "services"
	branch  = "master"
	repoURL = "github.com/micro/services"
	// DefaultVersion is the default version of the service
	// the assume if none is specified
	DefaultVersion = "latest"
	// DefaultNamespace is the default namespace of the services,
	// this will eventually be loaded from config
	DefaultNamespace = "go.micro"
	// how many commits to load at service startup
	// when we have no "latest" commit cached in memory
	commitsToInit = 1

	// The base image for our builds
	image = "docker.pkg.github.com/micro/services"

	// self is a reference to self to prevent scheduler
	// from carelessly redeploying itself in the middle of
	// a deploy flow
	self = "platform/scheduler"

	// the runtime to use
	Runtime = runtime.DefaultRuntime
)

type serviceStatus string

var (
	serviceStatusCreated serviceStatus = "created"
	serviceStatusUpdated serviceStatus = "updated"
	serviceStatusDeleted serviceStatus = "deleted"
)

type githubFileChangeStatus string

// a list of github file status changes.
// not documented in the github API
var (
	githubFileChangeStatusCreated  githubFileChangeStatus = "added"
	githubFileChangeStatusChanged  githubFileChangeStatus = "changed"
	githubFileChangeStatusModified githubFileChangeStatus = "modified"
	githubFileChangeStatusRemoved  githubFileChangeStatus = "removed"
)

type fileToStatus struct {
	fileName string
	status   githubFileChangeStatus
}

type manager struct {
	// workflow file name
	workflow string
	// latest commit hash
	latestCommit string
	// last updated value of the latest processed workflow run
	lastUpdated time.Time

	// the github client
	// TODO: pluggable source
	client *github.Client
}

// returns a map key -> values of serviceName -> serviceStatus
func (m *manager) getChangedFolders(commitHash string) (map[string]serviceStatus, error) {
	log.Infof("Listing files for commit %v", commitHash)
	commit, _, err := m.client.Repositories.GetCommit(context.Background(), owner, repo, commitHash)
	if err != nil {
		return nil, err
	}
	if len(commit.Files) == 0 {
		log.Info("No files for diff")
	}
	filesToStatuses := []fileToStatus{}
	for _, v := range commit.Files {
		filesToStatuses = append(filesToStatuses, fileToStatus{
			fileName: v.GetFilename(),
			status:   githubFileChangeStatus(v.GetStatus()),
		})
	}
	return folderStatuses(filesToStatuses), nil
}

// maps github file change statuses to folders and their deployment status
// ie. "asim/scheduler/main.go" "removed" will become "asim/scheduler" "deleted"
func folderStatuses(statuses []fileToStatus) map[string]serviceStatus {
	folders := map[string]serviceStatus{}
	// Prioritize main.go creates and deletes
	for _, status := range statuses {
		fname := status.fileName
		status := status.status
		if !strings.HasSuffix(fname, "main.go") {
			continue
		}
		fold := path.Dir(fname)

		_, exists := folders[fold]
		if exists {
			continue
		}
		if status == "added" {
			folders[fold] = serviceStatusCreated
		} else if status == "removed" {
			folders[fold] = serviceStatusDeleted
		}

	}
	// continue with normal file changes for service updates
	for _, status := range statuses {
		fname := status.fileName
		// All service files are inside folders,
		// so any file in the top folder can be safely ignored.
		if !strings.Contains(fname, "/") {
			continue
		}
		folds := topFolders(fname)
		for _, fold := range folds {
			_, exists := folders[fold]
			if exists {
				continue
			}
			folders[fold] = serviceStatusUpdated
		}
	}
	return folders
}

// from path returns the top level dirs to be deployed
// ie.
func topFolders(path string) []string {
	parts := strings.Split(path, "/")
	ret := []string{parts[0]}
	if len(parts) > 2 {
		ret = append(ret, filepath.Join(parts[0], parts[1]))
	}
	return ret
}

func (m *manager) updateService(folderPath, commit, build string, status serviceStatus) error {
	service := &runtime.Service{
		Name:    folderPath,
		Source:  path.Join(repoURL, folderPath),
		Version: "latest",
		Metadata: map[string]string{
			"commit":      commit,
			"repo":        repoURL,
			"build":       build,
			"deployed_by": "go.micro.platform.scheduler",
		},
	}
	typ := typeFromFolder(folderPath)
	formattedName := strings.ReplaceAll(service.Name, "/", "-")
	img := fmt.Sprintf("%v/%v", image, formattedName)

	switch status {
	case serviceStatusCreated:
		opts := []runtime.CreateOption{
			// create a specific service type
			runtime.CreateType(typ),
			runtime.CreateImage(img),
		}
		log.Infof("Creating service %v", folderPath)
		if err := Runtime.Create(service, opts...); err != nil {
			return err
		}
		return nil
	case serviceStatusUpdated:
		log.Infof("Updating service %v", folderPath)
		if err := Runtime.Update(service); err != nil {
			return err
		}
		return nil
	case serviceStatusDeleted:
		log.Infof("Deleting service %v", folderPath)
		if err := Runtime.Delete(service); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("Unrecognized service status: '%v'", status)
}

func typeFromFolder(folder string) string {
	if strings.Contains(folder, "api") {
		return "api"
	}
	if strings.Contains(folder, "web") {
		return "web"
	}
	return "service"
}

func (m *manager) Run() {
	t := time.NewTicker(90 * time.Second)
	defer t.Stop()

	// every minute we look for changes and apply any updates
	for {
		select {
		case <-t.C:
			log.Info("Listing workflows")
			workflows, _, err := m.client.Actions.ListWorkflowRunsByFileName(
				context.Background(),
				owner,
				repo,
				m.workflow,
				&github.ListWorkflowRunsOptions{
					Status: "success",
					Branch: branch,
				},
			)
			if err != nil {
				log.Errorf("Error listing workflows: %v", err)
				continue
			}
			if len(workflows.WorkflowRuns) == 0 {
				log.Error("No workflows returned")
				continue
			}

			var processList []*github.WorkflowRun
			// If there is nothing in memory save the commit and continue
			if m.latestCommit == "" {
				// save the latest
				workflow := workflows.WorkflowRuns[0]
				m.latestCommit = workflow.GetHeadSHA()
				m.lastUpdated = workflow.GetUpdatedAt().Time
				continue
			} else {
				processList = workflows.WorkflowRuns
				reverse(processList)
			}

			// references to self
			var updateSelf *serviceStatus
			var workflowID int64
			var commitSha string

			for _, workflow := range processList {
				if m.lastUpdated.After(workflow.GetUpdatedAt().Time) ||
					m.lastUpdated.Equal(workflow.GetUpdatedAt().Time) {
					continue
				}
				if workflow.GetConclusion() != "success" {
					continue
				}
				commit := workflow.GetHeadSHA()
				log.Infof("Processing workflow run for commit %v", commit)

				folderStatuses, err := m.getChangedFolders(commit)
				if err != nil {
					log.Errorf("Can't get services from commit", err)
				}

				// perform an update
				for folder, status := range folderStatuses {
					if folder == self {
						// save the reference to self
						updateSelf = &status
						workflowID = workflow.GetID()
						commitSha = commit
						// continue now
						continue
					}

					if err := m.updateService(folder, commit, fmt.Sprintf("%v", workflow.GetID()), status); err != nil {
						log.Errorf("Error updating service '%v': %v", folder, err)
						continue
					}
				}

				// save the latest
				m.latestCommit = commit
				m.lastUpdated = workflow.GetUpdatedAt().Time
			}

			// if found reference to self then update
			if updateSelf != nil {
				if err := m.updateService(self, commitSha, fmt.Sprintf("%v", workflowID), *updateSelf); err != nil {
					log.Errorf("Error updating self '%v': %v", self, err)
					continue
				}
			}
		}
	}
}

func reverse(s []*github.WorkflowRun) {
	for i, j := 0, len(s)-1; i < j; i, j = i+1, j-1 {
		s[i], s[j] = s[j], s[i]
	}
}

// Start the scheduler
func Start(workflowFilename string) error {
	// @todod avoid these messy global usages
	configs := config.Get("micro", "platform", "scheduler").StringMap(map[string]string{})
	if repoConfig, ok := configs["repo"]; ok {
		repo = repoConfig
	}
	if ownerConfig, ok := configs["owner"]; ok {
		owner = ownerConfig
	}
	if branchConfig, ok := configs["branch"]; ok {
		branch = branchConfig
	}
	if repoURLConfig, ok := configs["repoURL"]; ok {
		repoURL = repoURLConfig
	}

	m := new(manager)
	m.workflow = workflowFilename
	m.client = github.NewClient(nil)

	go m.Run()
	return nil
}
