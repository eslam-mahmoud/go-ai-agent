package orchestrator

import (
	"fmt"
	"strings"
)

const clarificationPrefix = "NEEDS_CLARIFICATION:"

func detectClarification(output string) (bool, string) {
	trimmed := strings.TrimSpace(output)
	if strings.HasPrefix(trimmed, clarificationPrefix) {
		return true, strings.TrimSpace(trimmed[len(clarificationPrefix):])
	}
	return false, ""
}

func BuildFirstRunPrompt(issueTitle, issueBody, threadComments string, issueNumber, maxBodyChars int) string {
	branch := fmt.Sprintf("madar/issue-%d", issueNumber)
	var sb strings.Builder
	sb.WriteString("You are working on the following GitHub Issue task. Complete the task fully and autonomously.\n\n")
	sb.WriteString("IMPORTANT RULES:\n")
	sb.WriteString(fmt.Sprintf("1. Create a branch named exactly `%s` for all your changes.\n", branch))
	sb.WriteString("2. Commit your changes to that branch and push it.\n")
	sb.WriteString("3. Open a pull request from that branch and include 'PR: #<number>' (e.g. 'PR: #42') on its own line in your final response so the CI watcher can track it.\n")
	sb.WriteString("4. If you need clarification before proceeding, respond with exactly:\n")
	sb.WriteString("   NEEDS_CLARIFICATION: <your question here>\n\n")
	sb.WriteString("Otherwise, complete the task and summarize what you did.\n\n")
	sb.WriteString("---\n")
	sb.WriteString("Title: ")
	sb.WriteString(issueTitle)
	sb.WriteString("\n\n")
	if issueBody != "" {
		body := issueBody
		if maxBodyChars > 0 && len(body) > maxBodyChars {
			body = body[:maxBodyChars] + "\n[truncated — see issue for full description]"
		}
		sb.WriteString("Description:\n")
		sb.WriteString(body)
		sb.WriteString("\n\n")
	}
	if threadComments != "" {
		sb.WriteString("Issue thread:\n")
		sb.WriteString(threadComments)
		sb.WriteString("\n")
	}
	return sb.String()
}

type ReplyEntry struct {
	Author    string
	Body      string
	Timestamp string
}

func BuildResumePrompt(replies []ReplyEntry) string {
	var sb strings.Builder
	if len(replies) == 1 {
		sb.WriteString("Maintainer replied:\n\n")
	} else {
		sb.WriteString(fmt.Sprintf("Maintainer replied (%d messages):\n\n", len(replies)))
	}
	for _, reply := range replies {
		sb.WriteString(fmt.Sprintf("@%s (%s):\n%s\n\n", reply.Author, reply.Timestamp, reply.Body))
	}
	sb.WriteString("Continue with the task based on this input.")
	return sb.String()
}

func BuildCIFixPrompt(failureOutput, branch string, prNumber, retryN, maxRetries int) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("CI failed on attempt %d of %d.\n\n", retryN, maxRetries))
	sb.WriteString(fmt.Sprintf(
		"IMPORTANT: Push your fix to the existing branch `%s` (PR #%d is already open). "+
			"Do NOT create a new branch or open a new PR.\n\n",
		branch, prNumber,
	))
	sb.WriteString("If you cannot fix the issue, respond with:\n")
	sb.WriteString("NEEDS_CLARIFICATION: <description of the problem>\n\n")
	sb.WriteString("--- CI Failure Output ---\n")
	sb.WriteString(failureOutput)
	sb.WriteString("\n------------------------\n\n")
	sb.WriteString("Diagnose the root cause, fix it, and push to the branch above.")
	return sb.String()
}
