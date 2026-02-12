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
	Number            int         `json:"number"`
	Title             string      `json:"title"`
	HeadRef           string      `json:"headRefName"`
	BaseRef           string      `json:"baseRefName"`
	URL               string      `json:"url"`
	IsDraft           bool        `json:"isDraft"`
	Author            Author      `json:"author"`
	Mergeable         string      `json:"mergeable"`
	MergeStateStatus  string      `json:"mergeStateStatus"`
	ReviewDecision    string      `json:"reviewDecision"`
	Labels            []Label     `json:"labels"`
	Checks            []Check     `json:"-"`
	StatusCheckRollup []checkNode `json:"statusCheckRollup"`
}

type Label struct {
	Name string `json:"name"`
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
	ID         string          `json:"id"`
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

type Review struct {
	Author string `json:"author"`
	State  string `json:"state"`
}

func (c *Client) ListOpenPRs(ctx context.Context, owner, repo string) ([]PRInfo, error) {
	args := []string{
		"pr", "list",
		"-R", owner + "/" + repo,
		"--json", "number,title,headRefName,baseRefName,url,isDraft,author,mergeable,mergeStateStatus,reviewDecision,labels,statusCheckRollup",
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
		"--json", "number,title,headRefName,baseRefName,url,isDraft,author,mergeable,mergeStateStatus,reviewDecision,labels,statusCheckRollup",
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
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []graphQLThread `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
}

type graphQLThread struct {
	ID         string `json:"id"`
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
	query := `query($owner: String!, $repo: String!, $pr: Int!, $cursor: String) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      reviewThreads(first: 100, after: $cursor) {
        pageInfo {
          hasNextPage
          endCursor
        }
        nodes {
          id
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

	var threads []ReviewThread
	cursor := ""

	for {
		args := []string{
			"api", "graphql",
			"-f", "owner=" + owner,
			"-f", "repo=" + repo,
			"-F", fmt.Sprintf("pr=%d", number),
			"-f", "query=" + query,
		}
		if cursor != "" {
			args = append(args, "-f", "cursor="+cursor)
		}

		out, err := c.gh(ctx, args...)
		if err != nil {
			return nil, fmt.Errorf("get review threads PR #%d: %w", number, err)
		}

		var resp graphQLResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			return nil, fmt.Errorf("parse review threads: %w", err)
		}

		for _, t := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
			rt := ReviewThread{
				ID:         t.ID,
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

		if !resp.Data.Repository.PullRequest.ReviewThreads.PageInfo.HasNextPage {
			break
		}
		cursor = resp.Data.Repository.PullRequest.ReviewThreads.PageInfo.EndCursor
	}

	return threads, nil
}

func (c *Client) GetReviews(ctx context.Context, owner, repo string, number int) ([]Review, error) {
	args := []string{
		"pr", "view", fmt.Sprintf("%d", number),
		"-R", owner + "/" + repo,
		"--json", "reviews",
	}

	out, err := c.gh(ctx, args...)
	if err != nil {
		return nil, fmt.Errorf("get reviews PR #%d: %w", number, err)
	}

	var resp struct {
		Reviews []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			State string `json:"state"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse reviews: %w", err)
	}

	reviews := make([]Review, 0, len(resp.Reviews))
	for _, r := range resp.Reviews {
		reviews = append(reviews, Review{
			Author: r.Author.Login,
			State:  r.State,
		})
	}

	return reviews, nil
}

func (c *Client) ResolveReviewThread(ctx context.Context, threadID string) error {
	mutation := `mutation($threadID: ID!) {
  resolveReviewThread(input: {threadId: $threadID}) {
    thread {
      id
      isResolved
    }
  }
}`

	args := []string{
		"api", "graphql",
		"-f", "threadID=" + threadID,
		"-f", "query=" + mutation,
	}

	_, err := c.gh(ctx, args...)
	if err != nil {
		return fmt.Errorf("resolve review thread %s: %w", threadID, err)
	}

	return nil
}

func (c *Client) UpdateBranch(ctx context.Context, owner, repo string, number int) error {
	mutation := `mutation($prID: ID!) {
  updatePullRequestBranch(input: {pullRequestId: $prID}) {
    pullRequest {
      id
    }
  }
}`

	// Get PR node ID first
	prIDQuery := `query($owner: String!, $repo: String!, $num: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $num) {
      id
    }
  }
}`

	args := []string{
		"api", "graphql",
		"-f", "owner=" + owner,
		"-f", "repo=" + repo,
		"-F", fmt.Sprintf("num=%d", number),
		"-f", "query=" + prIDQuery,
	}

	out, err := c.gh(ctx, args...)
	if err != nil {
		return fmt.Errorf("get PR ID: %w", err)
	}

	var resp struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					ID string `json:"id"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return fmt.Errorf("parse PR ID: %w", err)
	}

	prID := resp.Data.Repository.PullRequest.ID
	if prID == "" {
		return fmt.Errorf("PR ID not found")
	}

	// Update branch
	args = []string{
		"api", "graphql",
		"-f", "prID=" + prID,
		"-f", "query=" + mutation,
	}

	_, err = c.gh(ctx, args...)
	if err != nil {
		return fmt.Errorf("update branch: %w", err)
	}

	return nil
}

func (c *Client) PostComment(ctx context.Context, owner, repo string, number int, body string) error {
	args := []string{
		"pr", "comment", fmt.Sprintf("%d", number),
		"-R", owner + "/" + repo,
		"-b", body,
	}

	_, err := c.gh(ctx, args...)
	if err != nil {
		return fmt.Errorf("post comment on PR #%d: %w", number, err)
	}

	return nil
}

func (c *Client) GetComments(ctx context.Context, owner, repo string, number int) ([]string, error) {
	query := `query($owner: String!, $repo: String!, $pr: Int!) {
  repository(owner: $owner, name: $repo) {
    pullRequest(number: $pr) {
      comments(last: 100) {
        nodes {
          body
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
		return nil, fmt.Errorf("get comments PR #%d: %w", number, err)
	}

	var resp struct {
		Data struct {
			Repository struct {
				PullRequest struct {
					Comments struct {
						Nodes []struct {
							Body string `json:"body"`
						} `json:"nodes"`
					} `json:"comments"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, fmt.Errorf("parse comments: %w", err)
	}

	var comments []string
	for _, node := range resp.Data.Repository.PullRequest.Comments.Nodes {
		comments = append(comments, node.Body)
	}

	return comments, nil
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
