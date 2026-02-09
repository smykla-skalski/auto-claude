package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
)

type Client struct {
	logger *slog.Logger
}

func NewClient(logger *slog.Logger) *Client {
	return &Client{logger: logger}
}

type PRInfo struct {
	Number   int     `json:"number"`
	Title    string  `json:"title"`
	HeadRef  string  `json:"headRefName"`
	BaseRef  string  `json:"baseRefName"`
	URL      string  `json:"url"`
	IsDraft  bool    `json:"isDraft"`
	Author   Author  `json:"author"`
	Mergeable string `json:"mergeable"`
	MergeStateStatus string `json:"mergeStateStatus"`
	Checks   []Check `json:"-"`
	StatusCheckRollup []checkNode `json:"statusCheckRollup"`
}

type Author struct {
	Login string `json:"login"`
}

type checkNode struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	Context    string `json:"context"`
	State      string `json:"state"`
}

type Check struct {
	Name       string
	Status     string
	Conclusion string
}

type ReviewThread struct {
	IsResolved bool            `json:"isResolved"`
	IsOutdated bool            `json:"isOutdated"`
	Path       string          `json:"path"`
	Line       int             `json:"line"`
	Comments   []ReviewComment `json:"comments"`
}

type ReviewComment struct {
	Author string `json:"author"`
	Body   string `json:"body"`
}

func (c *Client) ListOpenPRs(ctx context.Context, owner, repo string) ([]PRInfo, error) {
	args := []string{
		"pr", "list",
		"-R", owner + "/" + repo,
		"--json", "number,title,headRefName,baseRefName,url,isDraft,author,mergeable,mergeStateStatus,statusCheckRollup",
		"--limit", "100",
	}

	out, err := c.gh(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("list PRs: %w", err)
	}

	var prs []PRInfo
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse PRs: %w", err)
	}

	for i := range prs {
		prs[i].Checks = normalizeChecks(prs[i].StatusCheckRollup)
	}

	return prs, nil
}

func (c *Client) GetPRDetail(ctx context.Context, owner, repo string, number int) (*PRInfo, error) {
	args := []string{
		"pr", "view", fmt.Sprintf("%d", number),
		"-R", owner + "/" + repo,
		"--json", "number,title,headRefName,baseRefName,url,isDraft,author,mergeable,mergeStateStatus,statusCheckRollup",
	}

	out, err := c.gh(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("get PR #%d: %w", number, err)
	}

	var pr PRInfo
	if err := json.Unmarshal(out, &pr); err != nil {
		return nil, fmt.Errorf("parse PR #%d: %w", number, err)
	}

	pr.Checks = normalizeChecks(pr.StatusCheckRollup)
	return &pr, nil
}

type graphQLResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []graphQLThread `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

type graphQLThread struct {
	IsResolved bool   `json:"isResolved"`
	IsOutdated bool   `json:"isOutdated"`
	Path       string `json:"path"`
	Line       int    `json:"line"`
	Comments   struct {
		Nodes []graphQLComment `json:"nodes"`
	} `json:"comments"`
}

type graphQLComment struct {
	Author struct {
		Login string `json:"login"`
	} `json:"author"`
	Body string `json:"body"`
}

func (c *Client) GetReviewThreads(ctx context.Context, owner, repo string, number int) ([]ReviewThread, error) {
	query := `query($owner: String!, $repo: String!, $pr: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100) {
        nodes {
          isResolved
          isOutdated
          path
          line
          comments(first: 100) {
            nodes {
              author { login }
              body
            }
          }
        }
      }
    }
  }
}`

	args := []string{
		"api", "graphql",
		"-f", "owner=" + owner,
		"-f", "repo=" + repo,
		"-F", fmt.Sprintf("pr=%d", number),
		"-f", "query=" + query,
	}

	out, err := c.gh(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("get review threads PR #%d: %w", number, err)
	}

	var resp graphQLResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse review threads: %w", err)
	}

	var threads []ReviewThread
	for _, t := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
		rt := ReviewThread{
			IsResolved: t.IsResolved,
			IsOutdated: t.IsOutdated,
			Path:       t.Path,
			Line:       t.Line,
		}
		for _, c := range t.Comments.Nodes {
			rt.Comments = append(rt.Comments, ReviewComment{
				Author: c.Author.Login,
				Body:   c.Body,
			})
		}
		threads = append(threads, rt)
	}

	return threads, nil
}

func (c *Client) MergePR(ctx context.Context, owner, repo string, number int, method string) error {
	args := []string{
		"pr", "merge", fmt.Sprintf("%d", number),
		"-R", owner + "/" + repo,
		"--delete-branch",
	}

	switch method {
	case "squash":
		args = append(args, "--squash")
	case "merge":
		args = append(args, "--merge")
	default:
		args = append(args, "--squash")
	}

	_, err := c.gh(ctx, args...)
	if err != nil {
		return fmt.Errorf("merge PR #%d: %w", number, err)
	}

	return nil
}

func (c *Client) gh(ctx context.Context, args ...string) ([]byte, error) {
	c.logger.Debug("gh", "args", strings.Join(args, " "))
	cmd := exec.CommandContext(ctx, "gh", args...)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("%w: %s", err, string(exitErr.Stderr))
		}
		return nil, err
	}
	return out, nil
}

func normalizeChecks(nodes []checkNode) []Check {
	var checks []Check
	for _, n := range nodes {
		name := n.Name
		if name == "" {
			name = n.Context
		}
		status := n.Status
		if status == "" {
			status = n.State
		}
		conclusion := n.Conclusion
		if conclusion == "" && n.State == "SUCCESS" {
			conclusion = "success"
		}
		if conclusion == "" && n.State == "FAILURE" {
			conclusion = "failure"
		}
		checks = append(checks, Check{
			Name:       name,
			Status:     strings.ToUpper(status),
			Conclusion: strings.ToLower(conclusion),
		})
	}
	return checks
}
