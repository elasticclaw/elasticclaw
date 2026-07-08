package integrations

import "strings"

// DefaultFactoryPRPolicy is exported for the pkg/hub call sites that
// remain until the workflows/ extraction.
const DefaultFactoryPRPolicy = "## PR Completion Policy\n\n" +
	"Unless the issue, factory/workflow instructions, template instructions, or a later explicit user instruction says not to create a PR, finish implementation by creating a pull request.\n\n" +
	"Before sending `[DONE]`:\n" +
	"1. Commit the finished work.\n" +
	"2. Push the branch.\n" +
	"3. Open a pull request for the branch.\n" +
	"4. Send exactly: `[DONE] https://github.com/org/repo/pull/N` with the actual PR URL.\n\n" +
	"If you cannot create a PR because credentials, repository access, or remotes are missing, do not send `[DONE]`. Report the blocker and the verification already completed.\n"

// AppendDefaultFactoryPRPolicy appends the canned PR policy section.
func AppendDefaultFactoryPRPolicy(b *strings.Builder) {
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	b.WriteString(DefaultFactoryPRPolicy)
}
