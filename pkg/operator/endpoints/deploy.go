/*
Copyright 2019 Cortex Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package endpoints

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/cortexlabs/cortex/pkg/lib/errors"
	"github.com/cortexlabs/cortex/pkg/lib/files"
	"github.com/cortexlabs/cortex/pkg/lib/pointer"
	"github.com/cortexlabs/cortex/pkg/lib/table"
	"github.com/cortexlabs/cortex/pkg/operator/api/context"
	"github.com/cortexlabs/cortex/pkg/operator/api/resource"
	"github.com/cortexlabs/cortex/pkg/operator/api/schema"
	"github.com/cortexlabs/cortex/pkg/operator/api/userconfig"
	"github.com/cortexlabs/cortex/pkg/operator/config"
	ocontext "github.com/cortexlabs/cortex/pkg/operator/context"
	"github.com/cortexlabs/cortex/pkg/operator/workloads"
)

func Deploy(w http.ResponseWriter, r *http.Request) {
	ignoreCache := getOptionalBoolQParam("ignoreCache", false, r)
	force := getOptionalBoolQParam("force", false, r)

	configBytes, err := files.ReadReqFile(r, "cortex.yaml")
	if err != nil {
		RespondError(w, errors.WithStack(err))
		return
	}

	if len(configBytes) == 0 {
		RespondError(w, ErrorFormFileMustBeProvided("cortex.yaml"))
		return
	}

	projectBytes, err := files.ReadReqFile(r, "project.zip")

	userconf, err := userconfig.New("cortex.yaml", configBytes)
	if err != nil {
		RespondError(w, err)
		return
	}

	err = userconf.Validate(projectBytes)
	if err != nil {
		RespondError(w, err)
		return
	}

	ctx, err := ocontext.New(userconf, projectBytes, ignoreCache)
	if err != nil {
		RespondError(w, err)
		return
	}

	err = workloads.PopulateWorkloadIDs(ctx)
	if err != nil {
		RespondError(w, err)
		return
	}

	existingCtx := workloads.CurrentContext(ctx.App.Name)

	fullCtxMatch := false
	if existingCtx != nil && existingCtx.ID == ctx.ID && context.APIResourcesAndComputesMatch(ctx, existingCtx) {
		fullCtxMatch = true
	}

	err = workloads.ValidateDeploy(ctx)
	if err != nil {
		RespondError(w, err)
		return
	}

	deploymentStatus, err := workloads.GetDeploymentStatus(ctx.App.Name)
	if err != nil {
		RespondError(w, err)
		return
	}

	isUpdating := deploymentStatus == resource.UpdatingDeploymentStatus

	if isUpdating {
		if fullCtxMatch {
			msg := deployResponseMessage(ResDeploymentUpToDateUpdating(ctx.App.Name), ctx, nil)
			Respond(w, schema.DeployResponse{Message: msg})
			return
		}
		if !force {
			msg := deployResponseMessage(ResDifferentDeploymentUpdating(ctx.App.Name), ctx, nil)
			Respond(w, schema.DeployResponse{Message: msg})
			return
		}
	}

	err = config.AWS.UploadMsgpackToS3(ctx, ctx.Key)
	if err != nil {
		RespondError(w, err, ctx.App.Name, "upload context")
		return
	}

	err = workloads.Run(ctx)
	if err != nil {
		RespondError(w, err)
		return
	}

	apisBaseURL, err := workloads.APIsBaseURL()
	if err != nil {
		RespondError(w, err)
		return
	}

	var updatingAPIs []string
	var baseMessage string
	if !isUpdating && !ignoreCache && existingCtx != nil && fullCtxMatch {
		baseMessage = ResDeploymentUpToDate(ctx.App.Name)
	} else {
		baseMessage, updatingAPIs = apiDiffMessage(existingCtx, ctx, apisBaseURL)
	}

	Respond(w, schema.DeployResponse{
		Context:     ctx,
		APIsBaseURL: apisBaseURL,
		Message:     deployResponseMessage(baseMessage, ctx, updatingAPIs),
	})
}

func apiDiffMessage(previousCtx *context.Context, currentCtx *context.Context, apisBaseURL string) (string, []string) {
	var newAPIs []context.API
	var updatedAPIs []context.API
	var deletedAPIs []context.API

	if previousCtx == nil {
		for _, api := range currentCtx.APIs {
			newAPIs = append(newAPIs, *api)
		}
	} else {
		for _, api := range currentCtx.APIs {
			if prevAPI, ok := previousCtx.APIs[api.Name]; ok {
				if api.ID != prevAPI.ID || api.Compute.ID() != prevAPI.Compute.ID() {
					updatedAPIs = append(updatedAPIs, *api)
				}
			} else {
				newAPIs = append(newAPIs, *api)
			}
		}

		for _, api := range previousCtx.APIs {
			if _, ok := currentCtx.APIs[api.Name]; ok {
				continue
			}
			deletedAPIs = append(deletedAPIs, *api)
		}
	}

	var updatingAPIs []string
	var strs []string
	for _, api := range newAPIs {
		strs = append(strs, ResCreatingAPI(api.Name))
		updatingAPIs = append(updatingAPIs, api.Name)
	}
	for _, api := range updatedAPIs {
		strs = append(strs, ResUpdatingAPI(api.Name))
		updatingAPIs = append(updatingAPIs, api.Name)
	}
	for _, api := range deletedAPIs {
		strs = append(strs, ResDeletingAPI(api.Name))
	}

	return strings.Join(strs, "\n"), updatingAPIs
}

func deployResponseMessage(baseMessage string, ctx *context.Context, updatingAPIs []string) string {
	apiName := "<api_name>"

	if len(updatingAPIs) == 1 {
		apiName = updatingAPIs[0]
	} else if len(updatingAPIs) == 0 && len(ctx.APIs) == 1 {
		for apiName = range ctx.APIs {
			break
		}
	}

	var items table.KeyValuePairs
	items.Add("cortex get", "(show deployment status)")
	items.Add(fmt.Sprintf("cortex get %s", apiName), "(show api info)")
	items.Add(fmt.Sprintf("cortex logs %s", apiName), "(stream api logs)")

	return baseMessage + "\n\n" + items.String(&table.KeyValuePairOpts{
		Delimiter: pointer.String(""),
		NumSpaces: pointer.Int(2),
	})
}
