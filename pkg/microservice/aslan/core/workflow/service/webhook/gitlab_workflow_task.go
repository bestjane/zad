/*
Copyright 2021 The KodeRover Authors.

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

package webhook

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/go-multierror"
	"github.com/xanzy/go-gitlab"
	"go.uber.org/zap"

	"github.com/koderover/zadig/pkg/microservice/aslan/config"
	commonmodels "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/models"
	commonrepo "github.com/koderover/zadig/pkg/microservice/aslan/core/common/repository/mongodb"
	commonservice "github.com/koderover/zadig/pkg/microservice/aslan/core/common/service"
	"github.com/koderover/zadig/pkg/microservice/aslan/core/common/service/scmnotify"
	environmentservice "github.com/koderover/zadig/pkg/microservice/aslan/core/environment/service"
	workflowservice "github.com/koderover/zadig/pkg/microservice/aslan/core/workflow/service/workflow"
	"github.com/koderover/zadig/pkg/setting"
	"github.com/koderover/zadig/pkg/shared/codehost"
	e "github.com/koderover/zadig/pkg/tool/errors"
	gitlabtool "github.com/koderover/zadig/pkg/tool/git/gitlab"
	"github.com/koderover/zadig/pkg/tool/log"
	"github.com/koderover/zadig/pkg/types"
	"github.com/koderover/zadig/pkg/types/permission"
	"github.com/koderover/zadig/pkg/util"
)

type gitlabMergeRequestDiffFunc func(event *gitlab.MergeEvent, id int) ([]string, error)

type gitlabMergeEventMatcher struct {
	diffFunc gitlabMergeRequestDiffFunc
	log      *zap.SugaredLogger
	workflow *commonmodels.Workflow
	event    *gitlab.MergeEvent
}

func (gmem *gitlabMergeEventMatcher) Match(hookRepo *commonmodels.MainHookRepo) (bool, error) {
	ev := gmem.event
	// TODO: match codehost
	if (hookRepo.RepoOwner + "/" + hookRepo.RepoName) == ev.ObjectAttributes.Target.PathWithNamespace {
		if !EventConfigured(hookRepo, config.HookEventPr) {
			return false, nil
		}
		isRegular := hookRepo.IsRegular
		if !isRegular && hookRepo.Branch != ev.ObjectAttributes.TargetBranch {
			return false, nil
		}
		if isRegular {
			if matched, _ := regexp.MatchString(hookRepo.Branch, ev.ObjectAttributes.TargetBranch); !matched {
				return false, nil
			}
		}
		hookRepo.Branch = ev.ObjectAttributes.TargetBranch
		if ev.ObjectAttributes.State == "opened" {
			var changedFiles []string
			changedFiles, err := gmem.diffFunc(ev, hookRepo.CodehostID)
			if err != nil {
				gmem.log.Warnf("failed to get changes of event %v, err:%s", ev, err)
				return false, err
			}
			gmem.log.Debugf("succeed to get %d changes in merge event", len(changedFiles))

			return MatchChanges(hookRepo, changedFiles), nil
		}
	}
	return false, nil
}

func (gmem *gitlabMergeEventMatcher) UpdateTaskArgs(
	product *commonmodels.Product, args *commonmodels.WorkflowTaskArgs, hookRepo *commonmodels.MainHookRepo, requestID string,
) *commonmodels.WorkflowTaskArgs {
	factory := &workflowArgsFactory{
		workflow: gmem.workflow,
		reqID:    requestID,
	}

	args = factory.Update(product, args, &types.Repository{
		CodehostID: hookRepo.CodehostID,
		RepoName:   hookRepo.RepoName,
		RepoOwner:  hookRepo.RepoOwner,
		Branch:     hookRepo.Branch,
		PR:         gmem.event.ObjectAttributes.IID,
	})

	return args
}

func createGitlabEventMatcher(
	event interface{}, diffSrv gitlabMergeRequestDiffFunc, workflow *commonmodels.Workflow, log *zap.SugaredLogger,
) gitEventMatcher {
	switch evt := event.(type) {
	case *gitlab.PushEvent:
		return &gitlabPushEventMatcher{
			workflow: workflow,
			log:      log,
			event:    evt,
		}
	case *gitlab.MergeEvent:
		return &gitlabMergeEventMatcher{
			diffFunc: diffSrv,
			log:      log,
			event:    evt,
			workflow: workflow,
		}
	}

	return nil
}

type gitlabPushEventMatcher struct {
	log      *zap.SugaredLogger
	workflow *commonmodels.Workflow
	event    *gitlab.PushEvent
}

func (gpem *gitlabPushEventMatcher) Match(hookRepo *commonmodels.MainHookRepo) (bool, error) {
	ev := gpem.event
	if (hookRepo.RepoOwner + "/" + hookRepo.RepoName) == ev.Project.PathWithNamespace {
		if !EventConfigured(hookRepo, config.HookEventPush) {
			return false, nil
		}

		isRegular := hookRepo.IsRegular
		if !isRegular && hookRepo.Branch != getBranchFromRef(ev.Ref) {
			return false, nil
		}
		if isRegular {
			if matched, _ := regexp.MatchString(hookRepo.Branch, getBranchFromRef(ev.Ref)); !matched {
				return false, nil
			}
		}
		hookRepo.Branch = getBranchFromRef(ev.Ref)

		var changedFiles []string
		detail, err := codehost.GetCodehostDetail(hookRepo.CodehostID)
		if err != nil {
			gpem.log.Errorf("GetCodehostDetail error: %s", err)
			return false, err
		}

		client, err := gitlabtool.NewClient(detail.Address, detail.OauthToken)
		if err != nil {
			gpem.log.Errorf("NewClient error: %s", err)
			return false, err
		}

		// compare??????????????????commit????????????????????????
		diffs, err := client.Compare(ev.ProjectID, ev.Before, ev.After)
		if err != nil {
			gpem.log.Errorf("Failed to get push event diffs, error: %s", err)
			return false, err
		}
		for _, diff := range diffs {
			changedFiles = append(changedFiles, diff.NewPath)
			changedFiles = append(changedFiles, diff.OldPath)
		}

		return MatchChanges(hookRepo, changedFiles), nil
	}

	return false, nil
}

func (gpem *gitlabPushEventMatcher) UpdateTaskArgs(
	product *commonmodels.Product, args *commonmodels.WorkflowTaskArgs, hookRepo *commonmodels.MainHookRepo, requestID string,
) *commonmodels.WorkflowTaskArgs {
	factory := &workflowArgsFactory{
		workflow: gpem.workflow,
		reqID:    requestID,
	}

	factory.Update(product, args, &types.Repository{
		CodehostID: hookRepo.CodehostID,
		RepoName:   hookRepo.RepoName,
		RepoOwner:  hookRepo.RepoOwner,
		Branch:     hookRepo.Branch,
	})

	return args
}

func TriggerWorkflowByGitlabEvent(event interface{}, baseURI, requestID string, log *zap.SugaredLogger) error {
	// TODO: cache workflow
	// 1. find configured workflow
	workflowList, err := commonrepo.NewWorkflowColl().List(&commonrepo.ListWorkflowOption{})
	if err != nil {
		log.Errorf("failed to list workflow %v", err)
		return err
	}

	mErr := &multierror.Error{}
	diffSrv := func(mergeEvent *gitlab.MergeEvent, codehostId int) ([]string, error) {
		return findChangedFilesOfMergeRequest(mergeEvent, codehostId)
	}

	var notification *commonmodels.Notification

	for _, workflow := range workflowList {
		if workflow.HookCtl == nil || !workflow.HookCtl.Enabled {
			continue
		}

		log.Debugf("find %d hooks in workflow %s", len(workflow.HookCtl.Items), workflow.Name)
		for _, item := range workflow.HookCtl.Items {
			if item.WorkflowArgs == nil {
				continue
			}

			// 2. match webhook
			matcher := createGitlabEventMatcher(event, diffSrv, workflow, log)
			if matcher == nil {
				continue
			}

			matches, err := matcher.Match(item.MainRepo)
			if err != nil {
				mErr = multierror.Append(mErr, err)
				continue
			}

			if !matches {
				log.Debugf("event not matches %v", item.MainRepo)
				continue
			}

			log.Infof("event match hook %v of %s", item.MainRepo, workflow.Name)
			namespace := strings.Split(item.WorkflowArgs.Namespace, ",")[0]
			opt := &commonrepo.ProductFindOptions{Name: workflow.ProductTmplName, EnvName: namespace}
			var prod *commonmodels.Product
			if prod, err = commonrepo.NewProductColl().Find(opt); err != nil {
				log.Warnf("can't find environment %s-%s", item.WorkflowArgs.Namespace, workflow.ProductTmplName)
				continue
			}

			isMergeRequest := false
			prID := 0
			var mergeRequestID, commitID string
			if ev, isPr := event.(*gitlab.MergeEvent); isPr {
				isMergeRequest = true
				prID = ev.ObjectAttributes.IID

				// ?????????merge request?????????webhook?????????????????????????????????
				// ??????????????????merge request?????????commit?????????commit???????????????????????????????????????????????????????????????
				mergeRequestID = strconv.Itoa(ev.ObjectAttributes.IID)
				commitID = ev.ObjectAttributes.LastCommit.ID
				autoCancelOpt := &AutoCancelOpt{
					MergeRequestID: mergeRequestID,
					CommitID:       commitID,
					TaskType:       config.WorkflowType,
					MainRepo:       item.MainRepo,
					WorkflowArgs:   item.WorkflowArgs,
				}
				err := AutoCancelTask(autoCancelOpt, log)
				if err != nil {
					log.Errorf("failed to auto cancel workflow task when receive event %v due to %v ", event, err)
					mErr = multierror.Append(mErr, err)
				}

				if notification == nil {
					notification, _ = scmnotify.NewService().SendInitWebhookComment(
						item.MainRepo, ev.ObjectAttributes.IID, baseURI, false, false, log,
					)

					// ????????? gitlab diff_note
					InitDiffNote(ev, item.MainRepo, log)
				}
			}

			if notification != nil {
				item.WorkflowArgs.NotificationID = notification.ID.Hex()
			}

			args := matcher.UpdateTaskArgs(prod, item.WorkflowArgs, item.MainRepo, requestID)
			args.MergeRequestID = mergeRequestID
			args.CommitID = commitID
			args.Source = setting.SourceFromGitlab
			args.CodehostID = item.MainRepo.CodehostID
			args.RepoOwner = item.MainRepo.RepoOwner
			args.RepoName = item.MainRepo.RepoName
			// 3. create task with args
			if item.WorkflowArgs.BaseNamespace == "" {
				if resp, err := workflowservice.CreateWorkflowTask(args, setting.WebhookTaskCreator, permission.AnonymousUserID, false, log); err != nil {
					log.Errorf("failed to create workflow task when receive push event %v due to %v ", event, err)
					mErr = multierror.Append(mErr, err)
					// ??????????????????????????????????????????????????????????????????
					_, err2 := scmnotify.NewService().SendErrWebhookComment(
						item.MainRepo, workflow, err, prID, baseURI, false, false, log,
					)
					if err2 != nil {
						log.Errorf("SendErrWebhookComment failed, product:%s, workflow:%s, err:%v", workflow.ProductTmplName, workflow.Name, err2)
					}
				} else {
					log.Infof("succeed to create task %v", resp)
				}
			} else if item.WorkflowArgs.BaseNamespace != "" && isMergeRequest {
				if err = CreateEnvAndTaskByPR(args, prID, requestID, log); err != nil {
					log.Infof("CreateRandomEnv err:%v", err)
				}
			}
		}
	}

	return mErr.ErrorOrNil()
}

func findChangedFilesOfMergeRequest(event *gitlab.MergeEvent, codehostID int) ([]string, error) {
	detail, err := codehost.GetCodehostDetail(codehostID)
	if err != nil {
		return nil, fmt.Errorf("failed to find codehost %d: %v", codehostID, err)
	}

	client, err := gitlabtool.NewClient(detail.Address, detail.OauthToken)
	if err != nil {
		log.Error(err)
		return nil, e.ErrCodehostListProjects.AddDesc(err.Error())
	}

	return client.ListChangedFiles(event)
}

// InitDiffNote ??????gitlab???????????????DiffNote????????????????????????
func InitDiffNote(ev *gitlab.MergeEvent, mainRepo *commonmodels.MainHookRepo, log *zap.SugaredLogger) error {
	commitID := ev.ObjectAttributes.LastCommit.ID
	body := "KodeRover CI ?????????..."

	// ??????gitlab api??????????????????
	detail, err := codehost.GetCodehostDetail(mainRepo.CodehostID)
	if err != nil {
		log.Errorf("GetCodehostDetail failed, codehost:%d, err:%v", mainRepo.CodehostID, err)
		return fmt.Errorf("failed to find codehost %d: %v", mainRepo.CodehostID, err)
	}
	cli, _ := gitlab.NewOAuthClient(detail.OauthToken, gitlab.WithBaseURL(detail.Address))

	opt := &commonrepo.DiffNoteFindOpt{
		CodehostID:     mainRepo.CodehostID,
		ProjectID:      mainRepo.RepoOwner + "/" + mainRepo.RepoName,
		MergeRequestID: ev.ObjectAttributes.IID,
	}
	dn, err := commonrepo.NewDiffNoteColl().Find(opt)
	if err == nil {
		// ???pr???DiffNote?????????????????????????????????commit???????????????
		if dn.CommitID == commitID {
			return nil
		}
		// ???????????????commit????????????body???resolved
		// ??????note body
		noteBodyOpt := &gitlab.UpdateMergeRequestDiscussionNoteOptions{
			Body: &body,
		}
		_, _, err = cli.Discussions.UpdateMergeRequestDiscussionNote(dn.Repo.ProjectID, dn.MergeRequestID, dn.DiscussionID, dn.NoteID, noteBodyOpt)
		if err != nil {
			log.Errorf("UpdateMergeRequestDiscussionNote failed, err:%v", err)
			return err
		}

		// ??????resolved??????
		resolved := false
		resolveOpt := &gitlab.UpdateMergeRequestDiscussionNoteOptions{
			Resolved: &resolved,
		}
		_, _, err = cli.Discussions.UpdateMergeRequestDiscussionNote(dn.Repo.ProjectID, dn.MergeRequestID, dn.DiscussionID, dn.NoteID, resolveOpt)
		if err != nil {
			log.Errorf("UpdateMergeRequestDiscussionNote failed, err:%v", err)
			return err
		}

		// ??????????????????
		dn.Resolved = resolved
		dn.Body = body
		err = commonrepo.NewDiffNoteColl().Update(dn.ObjectID.Hex(), commitID, dn.Body, dn.Resolved)
		if err != nil {
			log.Errorf("UpdateDiscussionInfo failed, err:%v", err)
			return err
		}

		return nil
	}

	// ??????????????????
	diffNote := &commonmodels.DiffNote{
		Repo: &commonmodels.RepoInfo{
			CodehostID: mainRepo.CodehostID,
			Source:     "gitlab",
			ProjectID:  mainRepo.RepoOwner + "/" + mainRepo.RepoName,
			Address:    detail.Address,
			OauthToken: detail.OauthToken,
		},
		MergeRequestID: ev.ObjectAttributes.IID,
		CommitID:       commitID,
		Body:           body,
	}

	createOpt := &gitlab.CreateMergeRequestDiscussionOptions{
		Body: &diffNote.Body,
	}

	discussion, _, err := cli.Discussions.CreateMergeRequestDiscussion(diffNote.Repo.ProjectID, diffNote.MergeRequestID, createOpt)
	if err != nil {
		log.Errorf("CreateMergeRequestDiscussion failed, err:%v", err)
		return err
	}

	diffNote.DiscussionID = discussion.ID
	if len(discussion.Notes) > 0 {
		diffNote.NoteID = discussion.Notes[0].ID
	}
	err = commonrepo.NewDiffNoteColl().Create(diffNote)
	if err != nil {
		log.Errorf("DiffNote.Create failed, err:%v", err)
		return err
	}

	return nil
}

var mutex sync.Mutex

// CreateEnvAndTaskByPR ??????pr???????????????????????????????????????????????????????????????????????????????????????????????????
func CreateEnvAndTaskByPR(workflowArgs *commonmodels.WorkflowTaskArgs, prID int, requestID string, log *zap.SugaredLogger) error {
	//?????????????????????????????????
	opt := &commonrepo.ProductFindOptions{Name: workflowArgs.ProductTmplName, EnvName: workflowArgs.BaseNamespace}
	baseProduct, err := commonrepo.NewProductColl().Find(opt)
	if err != nil {
		return fmt.Errorf("CreateEnvAndTaskByPR Product Find err:%v", err)
	}

	mutex.Lock()
	defer func() {
		mutex.Unlock()
	}()
	if baseProduct.Render != nil {
		if renderSet, _ := commonrepo.NewRenderSetColl().Find(&commonrepo.RenderSetFindOption{Name: baseProduct.Render.Name, Revision: baseProduct.Render.Revision}); renderSet != nil {
			baseProduct.Vars = renderSet.KVs
		}
	}

	envName := fmt.Sprintf("%s-%d-%s%s", "pr", prID, util.GetRandomNumString(3), util.GetRandomString(3))
	util.Clear(&baseProduct.ID)
	baseProduct.Namespace = commonservice.GetProductEnvNamespace(envName, workflowArgs.ProductTmplName, "")
	baseProduct.UpdateBy = setting.SystemUser
	baseProduct.EnvName = envName
	err = environmentservice.CreateProduct(setting.SystemUser, requestID, baseProduct, log)
	if err != nil {
		return fmt.Errorf("CreateEnvAndTaskByPR CreateProduct err:%v", err)
	}

	timeoutSeconds := config.ServiceStartTimeout()
	//??????????????????
	if err = WaitEnvCreate(timeoutSeconds, envName, workflowArgs, log); err != nil {
		return err
	}

	workflowArgs.Namespace = envName
	taskResp, err := workflowservice.CreateWorkflowTask(workflowArgs, setting.WebhookTaskCreator, permission.AnonymousUserID, false, log)
	if err != nil {
		return fmt.Errorf("CreateEnvAndTaskByPR CreateWorkflowTask err???%v ", err)
	}

	taskStatus := ""
	for {
		taskInfo, err := commonrepo.NewTaskColl().Find(taskResp.TaskID, taskResp.PipelineName, config.WorkflowType)
		if err != nil {
			log.Errorf("CreateEnvAndTaskByPR PipelineTask find err:%v ", err)
			time.Sleep(time.Second)
			continue
		}

		if taskInfo.Status == config.StatusFailed || taskInfo.Status == config.StatusPassed || taskInfo.Status == config.StatusTimeout || taskInfo.Status == config.StatusCancelled {
			taskStatus = string(taskInfo.Status)
			break
		} else {
			time.Sleep(time.Second)
		}
	}
	//?????????????????????????????????????????????????????????
	if workflowArgs.EnvRecyclePolicy == setting.EnvRecyclePolicyAlways || (workflowArgs.EnvRecyclePolicy == setting.EnvRecyclePolicyTaskStatus && taskStatus == string(config.StatusPassed)) {
		err = commonservice.DeleteProduct(setting.SystemUser, envName, workflowArgs.ProductTmplName, requestID, log)
		if err != nil {
			log.Errorf("CreateEnvAndTaskByPR DeleteProduct err:%v ", err)
			return err
		}
		//??????????????????
		if err = WaitEnvDelete(timeoutSeconds, envName, workflowArgs, log); err != nil {
			return err
		}
	}

	return nil
}

func WaitEnvCreate(timeoutSeconds int, envName string, workflowArgs *commonmodels.WorkflowTaskArgs, log *zap.SugaredLogger) error {
	timeout := false
	go func() {
		<-time.After(time.Duration(timeoutSeconds) * time.Second)
		timeout = true
	}()

	for {
		if timeout {
			return fmt.Errorf("WaitEnvCreate %s wait create envName:%s timeout in %d seconds", workflowArgs.ProductTmplName, envName, timeoutSeconds)
		}

		productResp, err := environmentservice.GetProduct(setting.SystemUser, envName, workflowArgs.ProductTmplName, log)
		if err != nil {
			log.Errorf("WaitEnvCreate Product find err:%v ", err)
			time.Sleep(time.Second)
			continue
		}
		prTaskInfo := &commonmodels.PrTaskInfo{
			ProductName:      workflowArgs.ProductTmplName,
			EnvStatus:        productResp.Status,
			EnvName:          envName,
			EnvRecyclePolicy: workflowArgs.EnvRecyclePolicy,
		}

		if err = scmnotify.NewService().UpdateEnvAndTaskWebhookComment(workflowArgs, prTaskInfo, log); err != nil {
			log.Errorf("WaitEnvCreate create product UpdateEnvAndTaskWebhookComment err:%v", err)
		}

		if productResp.Status == setting.PodRunning || productResp.Status == setting.PodUnstable || productResp.Status == setting.ClusterUnknown {
			break
		} else {
			time.Sleep(time.Second)
		}
	}
	return nil
}

func WaitEnvDelete(timeoutSeconds int, envName string, workflowArgs *commonmodels.WorkflowTaskArgs, log *zap.SugaredLogger) error {
	timeout := false
	go func() {
		<-time.After(time.Duration(timeoutSeconds) * time.Second)
		timeout = true
	}()
	for {
		if timeout {
			return fmt.Errorf("WaitEnvDelete %s wait delete envName:%s timeout in %d seconds", workflowArgs.ProductTmplName, envName, timeoutSeconds)
		}

		prTaskInfo := &commonmodels.PrTaskInfo{
			ProductName:      workflowArgs.ProductTmplName,
			EnvName:          envName,
			EnvRecyclePolicy: workflowArgs.EnvRecyclePolicy,
		}
		productResp, err := environmentservice.GetProduct(setting.SystemUser, envName, workflowArgs.ProductTmplName, log)
		if err != nil {
			log.Errorf("WaitEnvDelete GetProduct err:%v ", err)
			prTaskInfo.EnvStatus = "Completed"
			if err = scmnotify.NewService().UpdateEnvAndTaskWebhookComment(workflowArgs, prTaskInfo, log); err != nil {
				log.Errorf("WaitEnvDelete delete product UpdateEnvAndTaskWebhookComment1 err:%v", err)
			}
			break
		}
		prTaskInfo.EnvStatus = productResp.Status
		if err = scmnotify.NewService().UpdateEnvAndTaskWebhookComment(workflowArgs, prTaskInfo, log); err != nil {
			log.Errorf("WaitEnvDelete delete product UpdateEnvAndTaskWebhookComment2 err:%v", err)
		}
		time.Sleep(time.Second)
	}
	return nil
}
