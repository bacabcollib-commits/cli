package view

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/internal/browser"
	"github.com/cli/cli/v2/internal/ghinstance"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/internal/prompter"
	"github.com/cli/cli/v2/internal/text"
	"github.com/cli/cli/v2/pkg/cmd/agent-task/capi"
	"github.com/cli/cli/v2/pkg/cmd/agent-task/shared"
	prShared "github.com/cli/cli/v2/pkg/cmd/pr/shared"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/spf13/cobra"
)

const (
	defaultLimit           = 40
	defaultLogPollInterval = 5 * time.Second
)

type ViewOptions struct {
	IO         *iostreams.IOStreams
	BaseRepo   func() (ghrepo.Interface, error)
	CapiClient func() (capi.CapiClient, error)
	HttpClient func() (*http.Client, error)
	Finder     prShared.PRFinder
	Prompter   prompter.Prompter
	Browser    browser.Browser

	LogRenderer func() shared.LogRenderer
	Sleep       func(d time.Duration)

	SelectorArg string
	PRNumber    int
	SessionID   string
	Web         bool
	Log         bool
	Follow      bool
}

func defaultLogRenderer() shared.LogRenderer {
	return shared.NewLogRenderer()
}

func NewCmdView(f *cmdutil.Factory, runF func(*ViewOptions) error) *cobra.Command {
	opts := &ViewOptions{
		IO:          f.IOStreams,
		HttpClient:  f.HttpClient,
		CapiClient:  shared.CapiClientFunc(f),
		Prompter:    f.Prompter,
		Browser:     f.Browser,
		LogRenderer: defaultLogRenderer,
		Sleep:       time.Sleep,
	}

	cmd := &cobra.Command{
		Use:   "view [<session-id> | <pr-number> | <pr-url> | <pr-branch>]",
		Short: "View an agent task session (preview)",
		Long: heredoc.Doc(`
			View an agent task session.
		`),
		Example: heredoc.Doc(`
			# View an agent task by session ID
			$ gh agent-task view e2fa49d2-f164-4a56-ab99-498090b8fcdf

			# View an agent task by pull request number in current repo
			$ gh agent-task view 12345

			# View an agent task by pull request number
			$ gh agent-task view --repo OWNER/REPO 12345

			# View an agent task by pull request reference
			$ gh agent-task view OWNER/REPO#12345

			# View a pull request agents tasks in the browser
			$ gh agent-task view 12345 --web
		`),
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Support -R/--repo override
			opts.BaseRepo = f.BaseRepo

			if len(args) > 0 {
				opts.SelectorArg = args[0]
				if shared.IsSessionID(opts.SelectorArg) {
					opts.SessionID = opts.SelectorArg
				} else if sessionID, err := shared.ParseSessionIDFromURL(opts.SelectorArg); err == nil {
					opts.SessionID = sessionID
				}
			}

			if opts.SessionID == "" && !opts.IO.CanPrompt() {
				return fmt.Errorf("session ID is required when not running interactively")
			}

			if opts.Follow && !opts.Log {
				return cmdutil.FlagErrorf("--log is required when providing --follow")
			}

			if opts.Finder == nil {
				opts.Finder = prShared.NewFinder(f)
			}

			if runF != nil {
				return runF(opts)
			}
			return viewRun(opts)
		},
	}

	cmdutil.EnableRepoOverride(cmd, f)

	cmd.Flags().BoolVarP(&opts.Web, "web", "w", false, "Open agent task in the browser")
	cmd.Flags().BoolVar(&opts.Log, "log", false, "Show agent session logs")
	cmd.Flags().BoolVar(&opts.Follow, "follow", false, "Follow agent session logs")

	return cmd
}

func viewRun(opts *ViewOptions) error {
	capiClient, err := opts.CapiClient()
	if err != nil {
		return err
	}

	ctx := context.Background()
	cs := opts.IO.ColorScheme()

	opts.IO.StartProgressIndicatorWithLabel("Fetching agent session...")
	defer opts.IO.StopProgressIndicator()

	var session *capi.Session

	if opts.SessionID != "" {
		sess, err := capiClient.GetSession(ctx, opts.SessionID)
		if err != nil {
			if errors.Is(err, capi.ErrSessionNotFound) {
				fmt.Fprintln(opts.IO.ErrOut, "session not found")
				return cmdutil.SilentError
			}
			return err
		}

		opts.IO.StopProgressIndicator()

		if opts.Web {
			var webURL string
			if sess.PullRequest != nil {
				webURL = fmt.Sprintf("%s/agent-sessions/%s", sess.PullRequest.URL, url.PathEscape(sess.ID))
			} else {
				// Currently the web Copilot Agents home GUI does not support focusing
				// on a given session, so we should just navigate to the home page.
				webURL = capi.AgentsHomeURL
			}

			if opts.IO.IsStdoutTTY() {
				fmt.Fprintf(opts.IO.ErrOut, "Opening %s in your browser.\n", text.DisplayURL(webURL))
			}
			return opts.Browser.Browse(webURL)
		}

		session = sess
	} else {
		var prID int64
		var prURL string

		if opts.SelectorArg != "" {
			// Finder does not support the PR/issue reference format (e.g. owner/repo#123)
			// so we need to check if the selector arg is a reference and fetch the PR
			// directly.
			if repo, num, err := prShared.ParseFullReference(opts.SelectorArg); err == nil {
				// Since the selector was a reference (i.e. without hostname data), we need to
				// check the base repo to get the hostname.
				baseRepo, err := opts.BaseRepo()
				if err != nil {
					return err
				}

				hostname := baseRepo.RepoHost()
				if hostname != ghinstance.Default() {
					return fmt.Errorf("agent tasks are not supported on this host: %s", hostname)
				}

				prID, prURL, err = capiClient.GetPullRequestDatabaseID(ctx, hostname, repo.RepoOwner(), repo.RepoName(), num)
				if err != nil {
					return fmt.Errorf("failed to fetch pull request: %w", err)
				}
			}
		}

		if prID == 0 {
			findOptions := prShared.FindOptions{
				Selector: opts.SelectorArg,
				Fields:   []string{"id", "url", "fullDatabaseId"},
			}

			pr, repo, err := opts.Finder.Find(findOptions)
			if err != nil {
				return err
			}

			if repo.RepoHost() != ghinstance.Default() {
				return fmt.Errorf("agent tasks are not supported on this host: %s", repo.RepoHost())
			}

			databaseID, err := strconv.ParseInt(pr.FullDatabaseID, 10, 64)
			if err != nil {
				return fmt.Errorf("failed to parse pull request: %w", err)
			}

			prID = databaseID
			prURL = pr.URL
		}

		// TODO(babakks): currently we just fetch a pre-defined number of
		// matching sessions to avoid hitting the API too many times, but it's
		// technically possible for a PR to be associated with lots of sessions
		// (i.e. above our selected limit).
		sessions, err := capiClient.ListSessionsByResourceID(ctx, "pull", prID, defaultLimit)
		if err != nil {
			return fmt.Errorf("failed to list sessions for pull request: %w", err)
		}

		if len(sessions) == 0 {
			fmt.Fprintln(opts.IO.ErrOut, "no session found for pull request")
			return cmdutil.SilentError
		}

		opts.IO.StopProgressIndicator()

		if opts.Web {
			// Note that, we needed to make sure the PR exists and it has at least one session
			// associated with it, other wise the `/agent-sessions` page would display the 404
			// error.

			// We don't need to navigate to a specific session; if there's only one session
			// then the GUI will automatically show it, otherwise the user can select from the
			// list. This is to avoid unnecessary prompting.
			webURL := prURL + "/agent-sessions"
			if opts.IO.IsStdoutTTY() {
				fmt.Fprintf(opts.IO.ErrOut, "Opening %s in your browser.\n", text.DisplayURL(webURL))
			}
			return opts.Browser.Browse(webURL)
		}

		session = sessions[0]
		if len(sessions) > 1 {
			now := time.Now()
			options := make([]string, 0, len(sessions))
			for _, session := range sessions {
				options = append(options, fmt.Sprintf(
					"%s %s • %s",
					shared.SessionSymbol(cs, session.State),
					session.Name,
					text.FuzzyAgo(now, session.CreatedAt),
				))
			}

			selected, err := opts.Prompter.Select("Select a session", "", options)
			if err != nil {
				return err
			}

			session = sessions[selected]
		}
	}

	printSession(opts, session)

	if opts.Log {
		return printLogs(opts, capiClient, session.ID)
	}
	return nil
}

func printSession(opts *ViewOptions, session *capi.Session) {
	cs := opts.IO.ColorScheme()

	if session.PullRequest != nil {
		fmt.Fprintf(opts.IO.Out, "%s • %s • %s%s\n",
			shared.ColorFuncForSessionState(*session, cs)(shared.SessionStateString(session.State)),
			cs.Bold(session.PullRequest.Title),
			session.PullRequest.Repository.NameWithOwner,
			cs.ColorFromString(prShared.ColorForPRState(*session.PullRequest))(fmt.Sprintf("#%d", session.PullRequest.Number)),
		)
	} else {
		// This can happen when the session is just created and a PR is not yet available for it
		fmt.Fprintf(opts.IO.Out, "%s\n", shared.ColorFuncForSessionState(*session, cs)(shared.SessionStateString(session.State)))
	}

	if session.User != nil {
		fmt.Fprintf(opts.IO.Out, "Started on behalf of %s %s\n", session.User.Login, text.FuzzyAgo(time.Now(), session.CreatedAt))
	} else {
		// Should never happen, but we need to cover the path
		fmt.Fprintf(opts.IO.Out, "Started %s\n", text.FuzzyAgo(time.Now(), session.CreatedAt))
	}

	if !opts.Log {
		fmt.Fprintln(opts.IO.Out, "")
		fmt.Fprintf(opts.IO.Out, "For detailed session logs, try:\ngh agent-task view '%s' --log\n", session.ID)
	} else if !opts.Follow {
		fmt.Fprintln(opts.IO.Out, "")
		fmt.Fprintf(opts.IO.Out, "To follow session logs, try:\ngh agent-task view '%s' --log --follow\n", session.ID)
	}

	if session.PullRequest != nil {
		fmt.Fprintln(opts.IO.Out, "")
		fmt.Fprintln(opts.IO.Out, cs.Muted("View this session on GitHub:"))
		fmt.Fprintln(opts.IO.Out, cs.Muted(fmt.Sprintf("%s/agent-sessions/%s", session.PullRequest.URL, url.PathEscape(session.ID))))
	}
}

func printLogs(opts *ViewOptions, capiClient capi.CapiClient, sessionID string) error {
	ctx := context.Background()

	renderer := opts.LogRenderer()

	if opts.Follow {
		var called bool
		fetcher := func() ([]byte, error) {
			if called {
				opts.Sleep(defaultLogPollInterval)
			}
			called = true
			raw, err := capiClient.GetSessionLogs(ctx, sessionID)
			if err != nil {
				return nil, err
			}
			return raw, nil
		}

		fmt.Fprintln(opts.IO.Out, "")
		return renderer.Follow(fetcher, opts.IO.Out, opts.IO)
	}

	raw, err := capiClient.GetSessionLogs(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("failed to fetch session logs: %w", err)
	}

	fmt.Fprintln(opts.IO.Out, "")
	_, err = renderer.Render(raw, opts.IO.Out, opts.IO)
	return err
}
