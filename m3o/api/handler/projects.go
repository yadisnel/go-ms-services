package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/micro/go-micro/v2"
	"github.com/micro/go-micro/v2/auth"
	"github.com/micro/go-micro/v2/errors"
	"github.com/micro/go-micro/v2/logger"

	kubernetes "github.com/micro/services/kubernetes/service/proto"
	pb "github.com/micro/services/m3o/api/proto"
	environments "github.com/micro/services/projects/environments/proto"
	invites "github.com/micro/services/projects/invite/proto"
	projects "github.com/micro/services/projects/service/proto"
	secrets "github.com/micro/services/secrets/service/proto"
	users "github.com/micro/services/users/service/proto"
)

// NewProjects returns an initialised projects handler
func NewProjects(service micro.Service) *Projects {
	return &Projects{
		name:         service.Name(),
		auth:         service.Options().Auth,
		users:        users.NewUsersService("go.micro.service.users", service.Client()),
		secrets:      secrets.NewSecretsService("go.micro.service.secrets", service.Client()),
		invites:      invites.NewInviteService("go.micro.service.projects.invite", service.Client()),
		projects:     projects.NewProjectsService("go.micro.service.projects", service.Client()),
		kubernetes:   kubernetes.NewKubernetesService("go.micro.service.kubernetes", service.Client()),
		environments: environments.NewEnvironmentsService("go.micro.service.projects.environments", service.Client()),
	}
}

// Projects implments the M3O project service proto
type Projects struct {
	name         string
	auth         auth.Auth
	users        users.UsersService
	secrets      secrets.SecretsService
	invites      invites.InviteService
	projects     projects.ProjectsService
	kubernetes   kubernetes.KubernetesService
	environments environments.EnvironmentsService
}

const (
	// imagePullSecretName is the name of the image pull secrets which contain the github
	// tokens needed for the runtime to pull the images from GitHub package repos.
	imagePullSecretName = "github-token"
)

// CreateProject and the underlying infra
func (p *Projects) CreateProject(ctx context.Context, req *pb.CreateProjectRequest, rsp *pb.CreateProjectResponse) error {
	// Validate the request
	if req.Project == nil {
		return errors.BadRequest(p.name, "Missing project")
	}

	// Identify the user
	userID, err := p.userIDFromContext(ctx)
	if err != nil {
		return err
	}

	// Validate the user has access to the github repo
	repos, err := p.listGitHubRepos(req.GithubToken)
	if err != nil {
		return err
	}
	var isMemberOfRepo bool
	for _, r := range repos {
		if r.Name == req.Project.Repository {
			isMemberOfRepo = true
			break
		}
	}
	if !isMemberOfRepo {
		return errors.BadRequest(p.name, "Must be a member of the repository")
	}

	// create the project
	cRsp, err := p.projects.Create(ctx, &projects.CreateRequest{
		Project: &projects.Project{
			Name:        strings.ToLower(req.Project.Name),
			Description: req.Project.Description,
			Repository:  req.Project.Repository,
		},
	})
	if err != nil {
		return err
	}

	// add the user as an owner
	_, err = p.projects.AddMember(ctx, &projects.AddMemberRequest{
		Role:      projects.Role_Owner,
		ProjectId: cRsp.Project.Id,
		Member: &projects.Member{
			Type: "user",
			Id:   userID,
		},
	})
	if err != nil {
		logger.Warnf("Error adding user to project: %v", err)
	}

	// write the github token to the secrets service
	_, err = p.secrets.Create(ctx, &secrets.CreateRequest{
		Path:  []string{"project", "token", cRsp.Project.Id},
		Value: req.GithubToken,
	})
	if err != nil {
		logger.Warnf("Error writing github token: %v", err)
	}

	// serialize the project
	rsp.Project = serializeProject(cRsp.Project)

	// generate the auth account for the webhooks
	rsp.ClientId, rsp.ClientSecret, err = p.generateCreds(cRsp.Project.Id, cRsp.Project.Name)
	return nil
}

// UpdateProject metadata
func (p *Projects) UpdateProject(ctx context.Context, req *pb.UpdateProjectRequest, rsp *pb.UpdateProjectResponse) error {
	// validate the request
	if req.Project == nil {
		return errors.BadRequest(p.name, "Missing project")
	}

	// find the project
	proj, err := p.findProject(ctx, req.Project.Id)
	if err != nil {
		return err
	}

	// assign the update attributes
	proj.Description = req.Project.Description

	// update the project
	_, err = p.projects.Update(ctx, &projects.UpdateRequest{Project: proj})
	return err
}

// ListProjects the user has access to
func (p *Projects) ListProjects(ctx context.Context, req *pb.ListProjectsRequest, rsp *pb.ListProjectsResponse) error {
	userID, err := p.userIDFromContext(ctx)
	if err != nil {
		return err
	}

	// get the projects the user is a member of
	tRsp, err := p.projects.ListMemberships(ctx, &projects.ListMembershipsRequest{
		Member: &projects.Member{Type: "user", Id: userID},
	})
	if err != nil {
		return err
	}

	rsp.Projects = make([]*pb.Project, 0, len(tRsp.Projects))
	for _, pr := range tRsp.Projects {
		// lookup the projects metadata and members
		pRsp, err := p.projects.Read(ctx, &projects.ReadRequest{Id: pr.Id})
		if err != nil {
			return err
		}
		proj := serializeProject(pRsp.Project)

		// lookup the environments
		eRsp, err := p.environments.Read(ctx, &environments.ReadRequest{ProjectId: pr.Id})
		if err == nil {
			proj.Environments = make([]*pb.Environment, 0, len(eRsp.Environments))
			for _, e := range eRsp.Environments {
				proj.Environments = append(proj.Environments, serializeEnvironment(e))
			}
		}

		// get the ids and roles of the members who are users
		var userIDs []string
		userRoles := make(map[string]projects.Role)
		for _, m := range pRsp.Project.Members {
			if m.Type != "user" {
				continue
			}
			userIDs = append(userIDs, m.Id)
			userRoles[m.Id] = m.Role
		}

		// get the metadata for the members
		uRsp, err := p.users.Read(ctx, &users.ReadRequest{Ids: userIDs})
		if err != nil {
			return err
		}
		proj.Members = make([]*pb.Member, 0, len(uRsp.Users))
		for _, u := range uRsp.Users {
			proj.Members = append(proj.Members, &pb.Member{
				Id:        u.Id,
				FirstName: u.FirstName,
				LastName:  u.LastName,
				Email:     u.Email,
				Role:      userRoles[u.Id].String(),
			})
		}

		// add the project to the result
		rsp.Projects = append(rsp.Projects, proj)
	}

	return nil
}

// ValidateProjectName validates a project name to ensure it is unique
func (p *Projects) ValidateProjectName(ctx context.Context, req *pb.ValidateProjectNameRequest, rsp *pb.ValidateProjectNameResponse) error {
	_, err := p.projects.Read(ctx, &projects.ReadRequest{Name: req.Name})
	if err == nil {
		return errors.BadRequest(p.name, "Name has already been taken")
	}
	return nil
}

// ValidateEnvironmentName validates a Environment name to ensure it is unique
func (p *Projects) ValidateEnvironmentName(ctx context.Context, req *pb.ValidateEnvironmentNameRequest, rsp *pb.ValidateEnvironmentNameResponse) error {
	eRsp, err := p.environments.Read(ctx, &environments.ReadRequest{ProjectId: req.ProjectId})
	if err != nil {
		return err
	}

	for _, env := range eRsp.Environments {
		if env.Name == req.Name {
			return errors.BadRequest(p.name, "Name has already been taken")
		}
	}

	return nil
}

// ValidateGithubToken takes a GitHub personal token and returns the repos it has access to
func (p *Projects) ValidateGithubToken(ctx context.Context, req *pb.ValidateGithubTokenRequest, rsp *pb.ValidateGithubTokenResponse) error {
	repos, err := p.listGitHubRepos(req.Token)
	if err != nil {
		return err
	}
	rsp.Repos = repos
	return nil
}

// WebhookAPIKey generates an auth account token which can be used to authenticate against the webhook api
func (p *Projects) WebhookAPIKey(ctx context.Context, req *pb.WebhookAPIKeyRequest, rsp *pb.WebhookAPIKeyResponse) error {
	// find the project
	proj, err := p.findProject(ctx, req.ProjectId)
	if err != nil {
		return err
	}

	// generate the auth account
	rsp.ClientId, rsp.ClientSecret, err = p.generateCreds(proj.Id, proj.Name)
	return err
}

// CreateEnvironment for a given project
func (p *Projects) CreateEnvironment(ctx context.Context, req *pb.CreateEnvironmentRequest, rsp *pb.CreateEnvironmentResponse) error {
	// validate the request
	if req.Environment == nil {
		return errors.BadRequest(p.name, "Missing environment")
	}
	if len(req.ProjectId) == 0 {
		return errors.BadRequest(p.name, "Missing project id")
	}

	// ensure the user has access to the project
	if _, err := p.findProject(ctx, req.ProjectId); err != nil {
		return errors.Forbidden(p.name, "Unable to access project")
	}

	// load the github token for the project
	sRsp, err := p.secrets.Read(ctx, &secrets.ReadRequest{
		Path: []string{"project", "token", req.ProjectId},
	})
	if err != nil {
		return errors.InternalServerError(p.name, "Error fetching github token: %v", err)
	}

	// create the environment
	env := &environments.Environment{
		ProjectId:   req.ProjectId,
		Name:        strings.ToLower(req.Environment.Name),
		Description: req.Environment.Description,
	}
	eRsp, err := p.environments.Create(ctx, &environments.CreateRequest{Environment: env})
	if err != nil {
		return errors.BadRequest(p.name, "Unable to create project: %v", err.Error())
	}

	// create the k8s namespace
	if _, err := p.kubernetes.CreateNamespace(ctx, &kubernetes.CreateNamespaceRequest{Name: eRsp.Environment.Namespace}); err != nil {
		p.environments.Delete(ctx, &environments.DeleteRequest{Id: eRsp.Environment.Id})
		return errors.BadRequest(p.name, "Unable to create k8s namespace: %v", err.Error())
	}

	// create the image pull secret the k8s service account will use
	ipsReq := &kubernetes.CreateImagePullSecretRequest{
		Namespace: eRsp.Environment.Namespace,
		Name:      imagePullSecretName,
		Token:     sRsp.Value,
	}
	if _, err := p.kubernetes.CreateImagePullSecret(ctx, ipsReq); err != nil {
		p.environments.Delete(ctx, &environments.DeleteRequest{Id: eRsp.Environment.Id})
		p.kubernetes.DeleteNamespace(ctx, &kubernetes.DeleteNamespaceRequest{Name: eRsp.Environment.Namespace})
		return errors.BadRequest(p.name, "Unable to create image pull secret: %v", err.Error())
	}

	// create the k8s service account using the github-token secret
	saReq := &kubernetes.CreateServiceAccountRequest{
		Namespace:        eRsp.Environment.Namespace,
		ImagePullSecrets: []string{imagePullSecretName},
	}
	if _, err := p.kubernetes.CreateServiceAccount(ctx, saReq); err != nil {
		p.environments.Delete(ctx, &environments.DeleteRequest{Id: eRsp.Environment.Id})
		p.kubernetes.DeleteNamespace(ctx, &kubernetes.DeleteNamespaceRequest{Name: eRsp.Environment.Namespace})
		p.kubernetes.DeleteImagePullSecret(ctx, &kubernetes.DeleteImagePullSecretRequest{
			Name: imagePullSecretName, Namespace: eRsp.Environment.Namespace,
		})
		return errors.BadRequest(p.name, "Unable to create service account: %v", err.Error())
	}

	rsp.Environment = serializeEnvironment(eRsp.Environment)
	return nil
}

// UpdateEnvironment metadata
func (p *Projects) UpdateEnvironment(ctx context.Context, req *pb.UpdateEnvironmentRequest, rsp *pb.UpdateEnvironmentResponse) error {
	// validate the request
	if req.Environment == nil {
		return errors.BadRequest(p.name, "Missing environment")
	}
	if len(req.Environment.Id) == 0 {
		return errors.BadRequest(p.name, "Missing environment id")
	}

	// lookup the environment
	rRsp, err := p.environments.Read(ctx, &environments.ReadRequest{Id: req.Environment.Id})
	if err != nil {
		return err
	}
	env := rRsp.Environment

	// ensure the user has access to the project
	if _, err := p.findProject(ctx, env.ProjectId); err != nil {
		return err
	}

	// assign the update attributes
	env.Description = req.Environment.Description

	// update the environment
	_, err = p.environments.Update(ctx, &environments.UpdateRequest{Environment: env})
	return err
}

// DeleteEnvironment and the underlying infra
func (p *Projects) DeleteEnvironment(ctx context.Context, req *pb.DeleteEnvironmentRequest, rsp *pb.DeleteEnvironmentRequest) error {
	// validate the request
	if len(req.Id) == 0 {
		return errors.BadRequest(p.name, "Missing id")
	}

	// lookup the environment
	rRsp, err := p.environments.Read(ctx, &environments.ReadRequest{Id: req.Id})
	if err != nil {
		return err
	}
	env := rRsp.Environment

	// ensure the user has access to the project
	if _, err := p.findProject(ctx, env.ProjectId); err != nil {
		return err
	}

	// delete the k8s image pull secrets
	ipsReq := &kubernetes.DeleteImagePullSecretRequest{Name: imagePullSecretName, Namespace: env.Namespace}
	if _, err := p.kubernetes.DeleteImagePullSecret(ctx, ipsReq); err != nil {
		return err
	}

	// delete the k8s service account
	saReq := &kubernetes.DeleteServiceAccountRequest{Namespace: env.Namespace}
	if _, err := p.kubernetes.DeleteServiceAccount(ctx, saReq); err != nil {
		return err
	}

	// delete the k8s namespace
	if _, err = p.kubernetes.DeleteNamespace(ctx, &kubernetes.DeleteNamespaceRequest{Name: env.Namespace}); err != nil {
		return err
	}

	// delete the environment
	_, err = p.environments.Delete(ctx, &environments.DeleteRequest{Id: env.Id})
	return err
}

// Invite someone to a project
func (p *Projects) Invite(ctx context.Context, req *pb.InviteRequest, rsp *pb.InviteResponse) error {
	// validate the user has access to the project
	if _, err := p.findProject(ctx, req.ProjectId); err != nil {
		return err
	}

	// send the invite
	_, err := p.invites.Generate(ctx, &invites.GenerateRequest{
		ProjectId: req.ProjectId, Name: req.Name, Email: req.Email,
	})
	return err
}

func (p *Projects) generateCreds(projectID, projectName string) (string, string, error) {
	id := fmt.Sprintf("%v-webhook-%v", projectName, time.Now().Unix())
	md := map[string]string{"project-id": projectID}

	acc, err := p.auth.Generate(id, auth.WithScopes("webhook"), auth.WithMetadata(md))
	if err != nil {
		return "", "", err
	}

	return acc.ID, acc.Secret, nil
}

func (p *Projects) userIDFromContext(ctx context.Context) (string, error) {
	acc, ok := auth.AccountFromContext(ctx)
	if !ok {
		return "", errors.Unauthorized(p.name, "Account Required")
	}

	uRsp, err := p.users.Read(ctx, &users.ReadRequest{Email: acc.ID})
	if err != nil {
		return "", errors.InternalServerError(p.name, "Auth error: %v", err)
	}

	return uRsp.User.Id, nil
}

func serializeProject(p *projects.Project) *pb.Project {
	return &pb.Project{
		Id:          p.Id,
		Name:        p.Name,
		Description: p.Description,
		Repository:  p.Repository,
	}
}

func serializeEnvironment(e *environments.Environment) *pb.Environment {
	return &pb.Environment{
		Id:          e.Id,
		Name:        e.Name,
		Namespace:   e.Namespace,
		Description: e.Description,
	}
}

func (p *Projects) listGitHubRepos(token string) ([]*pb.Repository, error) {
	r, _ := http.NewRequest("GET", "https://api.github.com/user/repos", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/vnd.github.nebula-preview+json")

	res, err := new(http.Client).Do(r)
	if err != nil {
		return nil, errors.InternalServerError(p.name, "Unable to connect to the GitHub API: %v", err)
	}

	if res.StatusCode == http.StatusUnauthorized {
		return nil, errors.BadRequest(p.name, "Invalid GitHub token")
	} else if res.StatusCode != http.StatusOK {
		return nil, errors.InternalServerError(p.name, "Unexpected status returned from the GitHub API: %v", res.Status)
	}

	bytes, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return nil, errors.InternalServerError(p.name, "Invalid response returned from the GitHub API: %v", err)
	}

	var repos []struct {
		Name    string `json:"full_name"`
		Private bool   `json:"private"`
	}
	if err := json.Unmarshal(bytes, &repos); err != nil {
		return nil, errors.InternalServerError(p.name, "Invalid response returned from the GitHub API: %v", err)
	}

	repoos := make([]*pb.Repository, 0, len(repos))
	for _, r := range repos {
		repoos = append(repoos, &pb.Repository{Name: strings.ToLower(r.Name), Private: r.Private})
	}

	return repoos, nil
}

func (p *Projects) findProject(ctx context.Context, id string) (*projects.Project, error) {
	// Identify the user
	userID, err := p.userIDFromContext(ctx)
	if err != nil {
		return nil, err
	}

	// get the projects the user belongs to
	mRsp, err := p.projects.ListMemberships(ctx, &projects.ListMembershipsRequest{
		Member: &projects.Member{Type: "user", Id: userID},
	})
	if err != nil {
		return nil, err
	}

	// check for membership
	var isMember bool
	for _, t := range mRsp.Projects {
		if t.Id == id {
			isMember = true
			break
		}
	}
	if !isMember {
		return nil, errors.Forbidden(p.name, "Not a member of this team")
	}

	// lookup the project
	rRsp, err := p.projects.Read(ctx, &projects.ReadRequest{Id: id})
	if err != nil {
		return nil, err
	}
	return rRsp.GetProject(), nil
}
