// Copyright 2014 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package v1

import (
	"net/url"
	"path"
	"strings"

	"github.com/Unknwon/com"

	sdk "github.com/go-gitea/go-sdk"

	"github.com/go-gitea/gitea/models"
	"github.com/go-gitea/gitea/modules/auth"
	"github.com/go-gitea/gitea/modules/base"
	"github.com/go-gitea/gitea/modules/log"
	"github.com/go-gitea/gitea/modules/middleware"
	"github.com/go-gitea/gitea/modules/setting"
)

// ToApiRepository converts repository to API format.
func ToApiRepository(owner *models.User, repo *models.Repository, permission sdk.Permission) *sdk.Repository {
	cl, err := repo.CloneLink()
	if err != nil {
		log.Error(4, "CloneLink: %v", err)
	}

	return &sdk.Repository{
		Id:          repo.Id,
		Owner:       *ToApiUser(owner),
		FullName:    owner.Name + "/" + repo.Name,
		Private:     repo.IsPrivate,
		Fork:        repo.IsFork,
		HtmlUrl:     setting.AppUrl + owner.Name + "/" + repo.Name,
		CloneUrl:    cl.HTTPS,
		SshUrl:      cl.SSH,
		Permissions: permission,
	}
}

func SearchRepos(ctx *middleware.Context) {
	opt := models.SearchOption{
		Keyword: path.Base(ctx.Query("q")),
		Uid:     com.StrTo(ctx.Query("uid")).MustInt64(),
		Limit:   com.StrTo(ctx.Query("limit")).MustInt(),
	}
	if opt.Limit == 0 {
		opt.Limit = 10
	}

	// Check visibility.
	if ctx.IsSigned && opt.Uid > 0 {
		if ctx.User.Id == opt.Uid {
			opt.Private = true
		} else {
			u, err := models.GetUserById(opt.Uid)
			if err != nil {
				ctx.JSON(500, map[string]interface{}{
					"ok":    false,
					"error": err.Error(),
				})
				return
			}
			if u.IsOrganization() && u.IsOwnedBy(ctx.User.Id) {
				opt.Private = true
			}
			// FIXME: how about collaborators?
		}
	}

	repos, err := models.SearchRepositoryByName(opt)
	if err != nil {
		ctx.JSON(500, map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}

	results := make([]*sdk.Repository, len(repos))
	for i := range repos {
		if err = repos[i].GetOwner(); err != nil {
			ctx.JSON(500, map[string]interface{}{
				"ok":    false,
				"error": err.Error(),
			})
			return
		}

		results[i] = &sdk.Repository{
			Id:       repos[i].Id,
			FullName: path.Join(repos[i].Owner.Name, repos[i].Name),
		}
	}

	ctx.JSON(200, map[string]interface{}{
		"ok":   true,
		"data": results,
	})
}

func createRepo(ctx *middleware.Context, owner *models.User, opt sdk.CreateRepoOption) {
	repo, err := models.CreateRepository(owner, opt.Name, opt.Description,
		opt.Gitignore, opt.License, opt.Private, false, opt.AutoInit)
	if err != nil {
		if err == models.ErrRepoAlreadyExist ||
			err == models.ErrRepoNameIllegal {
			ctx.JSON(422, &base.ApiJsonErr{err.Error(), base.DOC_URL})
		} else {
			log.Error(4, "CreateRepository: %v", err)
			if repo != nil {
				if err = models.DeleteRepository(ctx.User.Id, repo.Id, ctx.User.Name); err != nil {
					log.Error(4, "DeleteRepository: %v", err)
				}
			}
			ctx.Error(500)
		}
		return
	}

	ctx.JSON(200, ToApiRepository(owner, repo, sdk.Permission{true, true, true}))
}

// POST /user/repos
// https://developer.github.com/v3/repos/#create
func CreateRepo(ctx *middleware.Context, opt sdk.CreateRepoOption) {
	// Shouldn't reach this condition, but just in case.
	if ctx.User.IsOrganization() {
		ctx.JSON(422, "not allowed creating repository for organization")
		return
	}
	createRepo(ctx, ctx.User, opt)
}

// POST /orgs/:org/repos
// https://developer.github.com/v3/repos/#create
func CreateOrgRepo(ctx *middleware.Context, opt sdk.CreateRepoOption) {
	org, err := models.GetOrgByName(ctx.Params(":org"))
	if err != nil {
		if err == models.ErrUserNotExist {
			ctx.Error(404)
		} else {
			ctx.Error(500)
		}
		return
	}

	if !org.IsOwnedBy(ctx.User.Id) {
		ctx.Error(403)
		return
	}
	createRepo(ctx, org, opt)
}

func MigrateRepo(ctx *middleware.Context, form auth.MigrateRepoForm) {
	u, err := models.GetUserByName(ctx.Query("username"))
	if err != nil {
		if err == models.ErrUserNotExist {
			ctx.HandleAPI(422, err)
		} else {
			ctx.HandleAPI(500, err)
		}
		return
	}
	if !u.ValidtePassword(ctx.Query("password")) {
		ctx.HandleAPI(422, "Username or password is not correct.")
		return
	}

	ctxUser := u
	// Not equal means current user is an organization.
	if form.Uid != u.Id {
		org, err := models.GetUserById(form.Uid)
		if err != nil {
			if err == models.ErrUserNotExist {
				ctx.HandleAPI(422, err)
			} else {
				ctx.HandleAPI(500, err)
			}
			return
		}
		ctxUser = org
	}

	if ctx.HasError() {
		ctx.HandleAPI(422, ctx.GetErrMsg())
		return
	}

	if ctxUser.IsOrganization() {
		// Check ownership of organization.
		if !ctxUser.IsOwnedBy(u.Id) {
			ctx.HandleAPI(403, "Given user is not owner of organization.")
			return
		}
	}

	// Remote address can be HTTP/HTTPS/Git URL or local path.
	remoteAddr := form.CloneAddr
	if strings.HasPrefix(form.CloneAddr, "http://") ||
		strings.HasPrefix(form.CloneAddr, "https://") ||
		strings.HasPrefix(form.CloneAddr, "git://") {
		u, err := url.Parse(form.CloneAddr)
		if err != nil {
			ctx.HandleAPI(422, err)
			return
		}
		if len(form.AuthUsername) > 0 || len(form.AuthPassword) > 0 {
			u.User = url.UserPassword(form.AuthUsername, form.AuthPassword)
		}
		remoteAddr = u.String()
	} else if !com.IsDir(remoteAddr) {
		ctx.HandleAPI(422, "Invalid local path, it does not exist or not a directory.")
		return
	}

	repo, err := models.MigrateRepository(ctxUser, form.RepoName, form.Description, form.Private, form.Mirror, remoteAddr)
	if err != nil {
		if repo != nil {
			if errDelete := models.DeleteRepository(ctxUser.Id, repo.Id, ctxUser.Name); errDelete != nil {
				log.Error(4, "DeleteRepository: %v", errDelete)
			}
		}
		ctx.HandleAPI(500, err)
		return
	}

	log.Trace("Repository migrated: %s/%s", ctxUser.Name, form.RepoName)
	ctx.WriteHeader(200)
}

// GET /user/repos
// https://developer.github.com/v3/repos/#list-your-repositories
func ListMyRepos(ctx *middleware.Context) {
	ownRepos, err := models.GetRepositories(ctx.User.Id, true)
	if err != nil {
		ctx.JSON(500, &base.ApiJsonErr{"GetRepositories: " + err.Error(), base.DOC_URL})
		return
	}
	numOwnRepos := len(ownRepos)

	accessibleRepos, err := ctx.User.GetAccessibleRepositories()
	if err != nil {
		ctx.JSON(500, &base.ApiJsonErr{"GetAccessibleRepositories: " + err.Error(), base.DOC_URL})
		return
	}

	repos := make([]*sdk.Repository, numOwnRepos+len(accessibleRepos))
	for i := range ownRepos {
		repos[i] = ToApiRepository(ctx.User, ownRepos[i], sdk.Permission{true, true, true})
	}
	i := numOwnRepos

	for repo, access := range accessibleRepos {
		if err = repo.GetOwner(); err != nil {
			ctx.JSON(500, &base.ApiJsonErr{"GetOwner: " + err.Error(), base.DOC_URL})
			return
		}

		repos[i] = ToApiRepository(repo.Owner, repo, sdk.Permission{false, access >= models.ACCESS_MODE_WRITE, true})

		// FIXME: cache result to reduce DB query?
		if repo.Owner.IsOrganization() && repo.Owner.IsOwnedBy(ctx.User.Id) {
			repos[i].Permissions.Admin = true
		}
		i++
	}

	ctx.JSON(200, &repos)
}
