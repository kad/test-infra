/*
Copyright 2016 The Kubernetes Authors.

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

// TODO(spxtr): Refactor and test this properly.

package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Sirupsen/logrus"

	"k8s.io/test-infra/prow/github"
	"k8s.io/test-infra/prow/jenkins"
	"k8s.io/test-infra/prow/jobs"
	"k8s.io/test-infra/prow/kube"
	"k8s.io/test-infra/prow/line"
)

var (
	job       = flag.String("job-name", "", "Which Jenkins job to build.")
	repoOwner = flag.String("repo-owner", "", "Owner of the repo.")
	repoName  = flag.String("repo-name", "", "Name of the repo to test.")
	pr        = flag.Int("pr", 0, "Pull request to test.")
	baseRef   = flag.String("base-ref", "", "Target branch.")
	baseSHA   = flag.String("base-sha", "", "Base SHA of the PR.")
	pullSHA   = flag.String("pull-sha", "", "Head SHA of the PR.")
	refs      = flag.String("refs", "", "Refs to merge together, as expected by bootstrap.py.")

	namespace = flag.String("namespace", "default", "Namespace that we live in.")
	dryRun    = flag.Bool("dry-run", true, "Whether or not to make mutating GitHub/Jenkins calls.")
	report    = flag.Bool("report", true, "Whether or not to report the status on GitHub.")

	jobConfigs       = flag.String("job-config", "/etc/jobs/jobs", "Where the job-config configmap is mounted.")
	labelsPath       = flag.String("labels-path", "/etc/labels/labels", "Where our metadata.labels are mounted.")
	githubTokenFile  = flag.String("github-token-file", "/etc/github/oauth", "Path to the file containing the GitHub OAuth secret.")
	jenkinsURL       = flag.String("jenkins-url", "http://pull-jenkins-master:8080", "Jenkins URL")
	jenkinsUserName  = flag.String("jenkins-user", "jenkins-trigger", "Jenkins username")
	jenkinsTokenFile = flag.String("jenkins-token-file", "/etc/jenkins/jenkins", "Path to the file containing the Jenkins API token.")
)

const (
	guberBase = "https://k8s-gubernator.appspot.com/build/kubernetes-jenkins/pr-logs/pull"
	testInfra = "https://github.com/kubernetes/test-infra/issues"
)

type testClient struct {
	Job jobs.JenkinsJob

	RepoOwner string
	RepoName  string
	PRNumber  int
	BaseRef   string
	BaseSHA   string
	PullSHA   string
	Refs      string

	DryRun bool
	Report bool

	KubeJob       string
	KubeClient    *kube.Client
	JenkinsClient *jenkins.Client
	GitHubClient  githubClient
}

type githubClient interface {
	CreateStatus(owner, repo, ref string, s github.Status) error
	ListIssueComments(owner, repo string, number int) ([]github.IssueComment, error)
	CreateComment(owner, repo string, number int, comment string) error
	DeleteComment(owner, repo string, ID int) error
}

func main() {
	flag.Parse()
	logrus.SetFormatter(&logrus.JSONFormatter{})

	rand.Seed(time.Now().UTC().UnixNano())

	jenkinsSecretRaw, err := ioutil.ReadFile(*jenkinsTokenFile)
	if err != nil {
		logrus.WithError(err).Fatalf("Could not read token file.")
	}
	jenkinsToken := string(bytes.TrimSpace(jenkinsSecretRaw))

	var jenkinsClient *jenkins.Client
	if *dryRun {
		jenkinsClient = jenkins.NewDryRunClient(*jenkinsURL, *jenkinsUserName, jenkinsToken)
	} else {
		jenkinsClient = jenkins.NewClient(*jenkinsURL, *jenkinsUserName, jenkinsToken)
	}

	oauthSecretRaw, err := ioutil.ReadFile(*githubTokenFile)
	if err != nil {
		logrus.WithError(err).Fatalf("Could not read oauth secret file.")
	}
	oauthSecret := string(bytes.TrimSpace(oauthSecretRaw))

	var ghc *github.Client
	if *dryRun {
		ghc = github.NewDryRunClient(oauthSecret)
	} else {
		ghc = github.NewClient(oauthSecret)
	}

	kc, err := kube.NewClientInCluster(*namespace)
	if err != nil {
		logrus.Fatalf("Error getting client: %v", err)
	}

	kubeJob, err := getKubeJob(*labelsPath)
	if err != nil {
		logrus.Fatalf("Error getting kube job name: %v", err)
	}

	ja := jobs.JobAgent{}
	if err := ja.LoadOnce(*jobConfigs); err != nil {
		logrus.WithError(err).Fatal("Error loading job config.")
	}
	found, jenkinsJob := ja.GetJob(fmt.Sprintf("%s/%s", *repoOwner, *repoName), *job)
	if !found {
		logrus.Fatalf("Could not find job %s in job config.", *job)
	}

	client := &testClient{
		Job:       jenkinsJob,
		RepoOwner: *repoOwner,
		RepoName:  *repoName,
		PRNumber:  *pr,
		BaseRef:   *baseRef,
		BaseSHA:   *baseSHA,
		PullSHA:   *pullSHA,
		Refs:      *refs,

		DryRun: *dryRun,
		Report: *report,

		KubeJob:       kubeJob,
		KubeClient:    kc,
		JenkinsClient: jenkinsClient,
		GitHubClient:  ghc,
	}
	if jenkinsJob.Spec == nil {
		if err := client.TestPRJenkins(); err != nil {
			logrus.WithFields(fields(client)).WithError(err).Errorf("Error testing PR on Jenkins.")
		}
	} else {
		if err := client.TestPRKubernetes(); err != nil {
			logrus.WithFields(fields(client)).WithError(err).Errorf("Error testing PR on Kubernetes.")
		}
	}
}

func fields(c *testClient) logrus.Fields {
	return logrus.Fields{
		"job":      c.Job.Name,
		"org":      c.RepoOwner,
		"repo":     c.RepoName,
		"pr":       c.PRNumber,
		"base-ref": c.BaseRef,
		"base-sha": c.BaseSHA,
		"pull-sha": c.PullSHA,
		"refs":     c.Refs,
	}
}

// TestPRKubernetes starts a pod and watches it, updating GitHub status as
// necessary.
func (c *testClient) TestPRKubernetes() error {
	logrus.WithFields(fields(c)).Info("Starting pod.")
	// TODO(spxtr): Sequential build numbers.
	buildID := strconv.Itoa(rand.Int())
	spec := *c.Job.Spec
	for i := range spec.Containers {
		spec.Containers[i].Env = append(spec.Containers[i].Env,
			kube.EnvVar{
				Name:  "PULL_REFS",
				Value: c.Refs,
			},
			kube.EnvVar{
				Name:  "PULL_NUMBER",
				Value: strconv.Itoa(c.PRNumber),
			},
			kube.EnvVar{
				Name:  "PULL_BASE_REF",
				Value: c.BaseRef,
			},
			kube.EnvVar{
				Name:  "PULL_BASE_SHA",
				Value: c.BaseSHA,
			},
			kube.EnvVar{
				Name:  "PULL_PULL_SHA",
				Value: c.PullSHA,
			},
			kube.EnvVar{
				Name:  "BUILD_NUMBER",
				Value: buildID,
			},
		)
	}
	p := kube.Pod{
		Metadata: kube.ObjectMeta{
			Name: buildID,
		},
		Spec: spec,
	}
	actual, err := c.KubeClient.CreatePod(p)
	if err != nil {
		c.tryCreateStatus(github.StatusError, "Error creating build pod.", testInfra)
		return err
	}
	resultURL := c.guberURL(buildID)
	c.tryCreateStatus(github.StatusPending, "Build started", resultURL)
	for {
		po, err := c.KubeClient.GetPod(actual.Metadata.Name)
		if err != nil {
			c.tryCreateStatus(github.StatusError, "Error waiting for pod to complete.", testInfra)
			return err
		}
		if po.Status.Phase == kube.PodSucceeded {
			c.tryCreateStatus(github.StatusSuccess, "Build succeeded.", resultURL)
			break
		} else if po.Status.Phase == kube.PodFailed {
			c.tryCreateStatus(github.StatusFailure, "Build failed.", resultURL)
			c.tryCreateFailureComment(resultURL)
			break
		} else if po.Status.Phase == kube.PodUnknown {
			c.tryCreateStatus(github.StatusError, "Error watching build.", resultURL)
			break
		}
		time.Sleep(20 * time.Second)
	}
	return nil
}

// TestPRJenkins starts a Jenkins build and watches it, updating the GitHub
// status as necessary.
func (c *testClient) TestPRJenkins() error {
	if size, err := c.JenkinsClient.QueueSize(); err != nil {
		c.tryCreateStatus(github.StatusError, "Error checking Jenkins queue.", testInfra)
		return err
	} else if size > 200 {
		c.tryCreateStatus(github.StatusError, "Jenkins overloaded. Please try again later.", testInfra)
		return nil
	}
	logrus.WithFields(fields(c)).Info("Starting build.")
	c.tryCreateStatus(github.StatusPending, "Build triggered.", "")
	b, err := c.JenkinsClient.Build(jenkins.BuildRequest{
		JobName: c.Job.Name,
		Number:  c.PRNumber,
		Refs:    c.Refs,
		BaseRef: c.BaseRef,
		BaseSHA: c.BaseSHA,
		PullSHA: c.PullSHA,
	})
	if err != nil {
		c.tryCreateStatus(github.StatusError, "Error starting build.", testInfra)
		return err
	}
	eq, err := c.JenkinsClient.Enqueued(b)
	if err != nil {
		c.tryCreateStatus(github.StatusError, "Error queueing build.", testInfra)
		return err
	}
	for eq {
		time.Sleep(10 * time.Second)
		eq, err = c.JenkinsClient.Enqueued(b)
		if err != nil {
			c.tryCreateStatus(github.StatusError, "Error in queue.", testInfra)
			return err
		}
	}

	result, err := c.JenkinsClient.Status(b)
	if err != nil {
		c.tryCreateStatus(github.StatusError, "Error waiting for build.", testInfra)
		return err
	}

	resultURL := c.guberURL(strconv.Itoa(result.Number))
	c.tryCreateStatus(github.StatusPending, "Build started.", resultURL)
	for {
		if err != nil {
			c.tryCreateStatus(github.StatusError, "Error waiting for build.", testInfra)
			return err
		}
		if result.Building {
			time.Sleep(30 * time.Second)
		} else {
			if result.Success {
				c.tryCreateStatus(github.StatusSuccess, "Build succeeded.", resultURL)
				break
			} else {
				c.tryCreateStatus(github.StatusFailure, "Build failed.", resultURL)
				c.tryCreateFailureComment(resultURL)
				break
			}
		}
		result, err = c.JenkinsClient.Status(b)
	}
	return nil
}

func (c *testClient) guberURL(build string) string {
	url := guberBase
	if c.RepoOwner != "kubernetes" {
		url = fmt.Sprintf("%s/%s_%s", url, c.RepoOwner, c.RepoName)
	} else if c.RepoName != "kubernetes" {
		url = fmt.Sprintf("%s/%s", url, c.RepoName)
	}
	prName := strconv.Itoa(c.PRNumber)
	if prName == "0" {
		prName = "batch"
	}
	return fmt.Sprintf("%s/%s/%s/%s/", url, prName, c.Job.Name, build)
}

func (c *testClient) tryCreateStatus(state, desc, url string) {
	logrus.WithFields(fields(c)).WithFields(logrus.Fields{
		"state":       state,
		"description": desc,
		"url":         url,
	}).Info("Setting GitHub and Kubernetes status.")
	if c.Report {
		if err := c.GitHubClient.CreateStatus(c.RepoOwner, c.RepoName, c.PullSHA, github.Status{
			State:       state,
			Description: desc,
			Context:     c.Job.Context,
			TargetURL:   url,
		}); err != nil {
			logrus.WithFields(fields(c)).WithError(err).Error("Error setting GitHub status.")
		}
	}
	if err := line.SetJobStatus(c.KubeClient, c.KubeJob, state, desc, url); err != nil {
		logrus.WithFields(fields(c)).WithError(err).Error("Error setting Kube Job status.")
	}
}

func (c *testClient) tryCreateFailureComment(url string) {
	if !c.Report {
		return
	}
	ics, err := c.GitHubClient.ListIssueComments(c.RepoOwner, c.RepoName, c.PRNumber)
	if err != nil {
		logrus.WithFields(fields(c)).WithError(err).Error("Error listing issue comments.")
		return
	}
	for _, ic := range ics {
		if ic.User.Login != "k8s-ci-robot" {
			continue
		}
		if strings.HasPrefix(ic.Body, c.Job.Context) {
			if err := c.GitHubClient.DeleteComment(c.RepoOwner, c.RepoName, ic.ID); err != nil {
				logrus.WithFields(fields(c)).WithError(err).Error("Error deleting comment.")
			}
		}
	}
	// The deletion logic requires that it start with context.
	bodyFormat := `%s [**failed**](%s) for commit %s. [Full PR test history](http://pr-test.k8s.io/%d).

The magic incantation to run this job again is ` + "`%s`" + `. Please help us cut down flakes by linking to an [open flake issue](https://github.com/kubernetes/kubernetes/issues?q=is:issue+label:kind/flake+is:open) when you hit one in your PR.`
	body := fmt.Sprintf(bodyFormat, c.Job.Context, url, c.PullSHA, c.PRNumber, c.Job.RerunCommand)
	if err := c.GitHubClient.CreateComment(c.RepoOwner, c.RepoName, c.PRNumber, body); err != nil {
		logrus.WithFields(fields(c)).WithError(err).Error("Error creating comment.")
	}
}

func getKubeJob(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	re := regexp.MustCompile(`^job-name="([^"]+)"$`)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		m := re.FindStringSubmatch(scanner.Text())
		if len(m) == 2 {
			return m[1], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	return "", fmt.Errorf("could not find job-name in %s", path)
}
