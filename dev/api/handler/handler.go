package handler

import (
	"context"
	"fmt"

	"github.com/micro/go-micro/v2"
	"github.com/micro/go-micro/v2/auth"
	"github.com/micro/go-micro/v2/errors"

	apps "github.com/micro/services/apps/service/proto/apps"
	pb "github.com/micro/services/dev/api/proto/home"
	users "github.com/micro/services/users/service/proto"
)

// Handler implements the dev api interface
type Handler struct {
	name  string
	apps  apps.AppsService
	users users.UsersService
}

// NewHandler returns an initialised handler
func NewHandler(srv micro.Service) *Handler {
	return &Handler{
		name:  srv.Name(),
		apps:  apps.NewAppsService("go.micro.service.apps", srv.Client()),
		users: users.NewUsersService("go.micro.service.users", srv.Client()),
	}
}

// ReadUser returns information about the user currently logged in
func (h *Handler) ReadUser(ctx context.Context, req *pb.ReadUserRequest, rsp *pb.ReadUserResponse) error {
	acc, ok := auth.AccountFromContext(ctx)
	if !ok {
		return errors.Unauthorized(h.name, "account not found")
	}

	uRsp, err := h.users.Read(ctx, &users.ReadRequest{Id: acc.ID})
	if err != nil {
		return err
	}

	rsp.User = &pb.User{
		FirstName:         uRsp.User.FirstName,
		LastName:          uRsp.User.LastName,
		ProfilePictureUrl: uRsp.User.ProfilePictureUrl,
	}

	return nil
}

// ListApps returns all the apps a user has access to
func (h *Handler) ListApps(ctx context.Context, req *pb.ListAppsRequest, rsp *pb.ListAppsResponse) error {
	acc, ok := auth.AccountFromContext(ctx)
	if !ok {
		return errors.Unauthorized(h.name, "account not found")
	}

	aRsp, err := h.apps.List(ctx, &apps.ListRequest{OnlyActive: true})
	if err != nil {
		return err
	}

	rsp.Apps = make([]*pb.App, 0, len(aRsp.Apps))
	for _, a := range aRsp.Apps {
		// Ensure the user has the required roles
		// to access the app
		if !canAccessApp(acc, a) {
			continue
		}

		// Asset are served from root, e.g.icon.png
		// would become /distributed/icon.png
		var icon string
		if len(a.Icon) > 0 {
			icon = fmt.Sprintf("/%v/%v", a.Id, a.Icon)
		}

		rsp.Apps = append(rsp.Apps, &pb.App{
			Id:       a.Id,
			Name:     a.Name,
			Category: a.Category,
			Icon:     icon,
		})
	}

	return nil
}

func canAccessApp(acc *auth.Account, app *apps.App) bool {
	if len(app.Roles) == 0 {
		return true
	}

	for _, reqRole := range app.Roles {
		for _, accRole := range acc.Roles {
			if reqRole == accRole {
				return true
			}
		}
	}

	return false
}
