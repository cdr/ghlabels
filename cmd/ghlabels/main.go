package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/url"
	"os"
	"strings"

	hubgh "github.com/github/hub/github"
	"github.com/google/go-github/github"
	"go.nhooyr.io/ghlabels/internal/gh"
	"golang.org/x/oauth2"
)

func main() {
	log.SetFlags(0)

	usage := func() {
		log.Fatal(`usage: ghlabels <command> [args]

Commands:
  pull		pull labels from a repository
  push		push labelss
  rename	rename a label
  delete	delete a label`)
	}
	if len(os.Args) < 2 {
		usage()
	}

	ctx := context.Background()

	args := os.Args[2:]
	switch os.Args[1] {
	case "pull":
		pull(ctx, args)
	case "push":
		push(ctx, args)
	case "rename":
		rename(ctx, args)
	case "delete":
		deleteCmd(ctx, args)
	default:
		log.Printf("unknown sub command")
		usage()
	}
}

func newGithubClient() *github.Client {
	config := hubgh.CurrentConfig()
	h, err := config.DefaultHost()
	if err != nil {
		log.Fatalf("failed to get host from hub: %v", err)
	}

	ctx := context.Background()
	hc := oauth2.NewClient(ctx, oauth2.StaticTokenSource(&oauth2.Token{
		AccessToken: h.AccessToken,
	}))
	gc := github.NewClient(hc)

	gc.BaseURL = &url.URL{}
	gc.BaseURL.Scheme = h.Protocol

	if h.Host == "github.com" {
		gc.BaseURL.Host = "api.github.com"
		gc.BaseURL.Path = "/"
	} else {
		gc.BaseURL.Host = h.Host
		gc.BaseURL.Path = "/api/v3/"
	}
	return gc
}

func pull(ctx context.Context, args []string) {
	usage := func() {
		log.Fatal(`usage: ghlabels pull <org>/<repo>`)
	}
	if len(args) < 1 {
		usage()
	}

	gc := newGithubClient()

	repoPath := args[0]
	org, repo := splitRepoPath(repoPath)
	if repo == "" {
		log.Printf("invalid repo path %q", repoPath)
		usage()
	}

	ghlabels, err := gh.Labels(ctx, gc, org, repo)
	if err != nil {
		log.Fatalf("failed to pull %v: %v", repoPath, err)
	}

	labels := makeLabels(ghlabels)

	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "    ")
	err = e.Encode(labels)
	if err != nil {
		log.Fatalf("failed to encode labels to stdout: %v", err)
	}
}

func push(ctx context.Context, args []string) {
	usage := func() {
		log.Fatal(`usage: ghlabels push <owner>[/<repo>]`)
	}
	if len(args) < 1 {
		usage()
	}

	gc := newGithubClient()

	repoPath := args[0]
	owner, repos := expandRepoPath(ctx, gc, repoPath)

	var labels []*label
	d := json.NewDecoder(os.Stdin)
	err := d.Decode(&labels)
	if err != nil {
		log.Fatalf("failed to decode stdin into labels: %v", err)
	}

	for _, repo := range repos {
		for _, label := range labels {
			ghlabel := label.github()

			err = gh.CreateLabel(ctx, gc, owner, repo, ghlabel)
			if err == gh.ErrAlreadyExists {
				err = gh.EditLabel(ctx, gc, owner, repo, ghlabel.GetName(), ghlabel)
				if err != nil {
					log.Fatalf("failed to edit label %q on %v/%v: %v", label.Name, owner, repo, err)
				}
			}
			if err != nil {
				log.Fatalf("failed to create label %q on %v/%v: %v", label.Name, owner, repo, err)
			}
		}
	}
}

func rename(ctx context.Context, args []string) {
	usage := func() {
		log.Fatalf(`usage: ghlabels rename <owner>[/<repo>] <old_name> <new_name>`)
	}
	if len(args) < 3 {
		usage()
	}

	gc := newGithubClient()

	repoPath := args[0]
	oldLabel := args[1]
	newLabelName := args[2]

	org, repos := expandRepoPath(ctx, gc, repoPath)

	for _, repo := range repos {
		newLabel := &github.Label{
			Name: github.String(newLabelName),
		}
		err := gh.EditLabel(ctx, gc, org, repo, oldLabel, newLabel)
		if err != nil {
			log.Fatalf("failed to list labels for %v/%v: %v", org, repo, err)
		}
	}
}

func deleteCmd(ctx context.Context, args []string) {
	usage := func() {
		log.Fatalf(`usage: ghlabels delete [--defaults] [<label>] <org>[<repo>]

You must provide at least one of --defaults or <label>.`)
	}
	if len(args) < 2 {
		usage()
	}
	gc := newGithubClient()

	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	deleteDefaults := fs.Bool("defaults", false, "delete all default labels")
	err := fs.Parse(args)
	if err != nil {
		log.Printf("failed to parse flags: %v", err)
		usage()
	}

	args = fs.Args()
	if len(args) < 1 {
		usage()
	}
	if !*deleteDefaults && len(args) < 2 {
		log.Printf("you must provide either --defaults or a label")
		usage()
	}

	// This is safe even if label is never initialized because an
	// empty string will never match an existing label's name.
	var labelName string
	var org string
	var repos []string

	if len(args) < 2 {
		org = args[0]
	} else {
		labelName = args[0]
		org = args[1]
	}

	org, repos = expandRepoPath(ctx, gc, org)

	for _, repo := range repos {
		labels, err := gh.Labels(ctx, gc, org, repo)
		if err != nil {
			log.Fatalf("failed to list labels for %v/%v: %v", org, repo, err)
		}

		for _, label := range labels {
			if (label.GetDefault() && *deleteDefaults) || label.GetName() == labelName {
				err = gh.DeleteLabel(ctx, gc, org, repo, label.GetName())
				if err != nil {
					log.Fatalf("failed to delete label %q in %v/%v: %v", label.GetName(), org, repo, err)
				}
			}
		}
	}
}

func expandRepoPath(ctx context.Context, gc *github.Client, repoPath string) (org string, repos []string) {
	org, repo := splitRepoPath(repoPath)
	if repo == "" {
		h, _ := hubgh.CurrentConfig().DefaultHost()
		var ghrepos []*github.Repository
		var err error
		if org == h.User {
			ghrepos, err = gh.UserRepos(ctx, gc)
		} else {
			ghrepos, err = gh.OrgRepos(ctx, gc, org)
		}
		if err != nil {
			log.Fatalf("failed to list repos for %q: %v", org, err)
		}

		for _, gr := range ghrepos {
			repos = append(repos, gr.GetName())
		}
	} else {
		repos = []string{repo}
	}

	return org, repos
}

func splitRepoPath(repoPath string) (owner, repo string) {
	s := strings.SplitN(repoPath, "/", 2)
	if len(s) < 2 {
		return s[0], ""
	}
	return s[0], s[1]
}