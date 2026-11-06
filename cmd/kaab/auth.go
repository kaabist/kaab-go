package main

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"github.com/kaabist/kaab-go/kaab"
	"github.com/spf13/cobra"
)

func (a *app) authCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "auth", Short: "Store, show and forget API keys"}

	var key, org, panel string
	var fileStorage bool
	login := &cobra.Command{
		Use:   "login",
		Short: "Check an API key and store it in a profile",
		Long:  "Checks the key against the API (GET /org) and stores it in the OS keychain, or in\n~/.config/kaab/credentials.toml (0600) when there is no keychain. A profile holds one live and\none test key; --test then selects the test one.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if key == "" {
				_, _ = fmt.Fprint(a.env.stderr, "API key (sk_live_… or sk_test_…): ")
				line, err := bufio.NewReader(a.env.stdin).ReadString('\n')
				if err != nil && line == "" {
					return errors.New("no key given: pass --key or type it")
				}
				key = strings.TrimSpace(line)
			}
			mode, err := keyMode(key)
			if err != nil {
				return err
			}
			s, err := a.store()
			if err != nil {
				return err
			}
			cfg, err := s.load()
			if err != nil {
				return err
			}
			name := a.profileName(cfg)
			profile := cfg.Profiles[name]
			if profile == nil {
				profile = &Profile{}
				cfg.Profiles[name] = profile
			}
			if a.g.baseURL != "" {
				profile.BaseURL = a.g.baseURL
			}
			if panel != "" {
				profile.PanelURL = panel
			}
			o, err := kaab.New(key, a.clientOptions(profile)...).Org.Get(cmd.Context())
			if err != nil {
				return fmt.Errorf("the API refused the key: %w", err)
			}
			if org != "" {
				profile.Org = org
			} else if profile.Org == "" {
				profile.Org = o.Name
			}
			where, err := s.putKey(name, mode, key, fileStorage)
			if err != nil {
				return err
			}
			if profile.Storage == nil {
				profile.Storage = StorageByMode{}
			}
			profile.Storage[mode] = where
			if !hasMode(profile, mode) {
				profile.Modes = append(profile.Modes, mode)
			}
			if cfg.Current == "" {
				cfg.Current = name
			}
			if err := s.save(cfg); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(a.env.stdout, "Logged in to %s (%s) with a %s key, profile %q; the key is in the %s.\n",
				o.Name, o.ID, mode, name, map[string]string{"keyring": "OS keychain", "file": "file " + s.credentialsPath()}[where])
			return nil
		},
	}
	login.Flags().StringVar(&key, "key", "", "the API key (read from stdin when absent)")
	login.Flags().StringVar(&org, "org", "", "a note naming the organisation (its name by default)")
	login.Flags().StringVar(&panel, "panel-url", "", "the panel's URL, for `kaab open` (https://app.kaab.ist by default)")
	login.Flags().BoolVar(&fileStorage, "file-storage", false, "keep the key in credentials.toml rather than the OS keychain")

	logout := &cobra.Command{
		Use:   "logout",
		Short: "Forget a profile and its keys",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			s, err := a.store()
			if err != nil {
				return err
			}
			cfg, err := s.load()
			if err != nil {
				return err
			}
			name := a.profileName(cfg)
			if err := s.deleteKeys(name); err != nil {
				return err
			}
			delete(cfg.Profiles, name)
			if cfg.Current == name {
				cfg.Current = ""
			}
			if err := s.save(cfg); err != nil {
				return err
			}
			_, _ = fmt.Fprintf(a.env.stdout, "Logged out of profile %q.\n", name)
			return nil
		},
	}

	status := &cobra.Command{
		Use:   "status",
		Short: "Show the profiles and check the selected one's key",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := a.store()
			if err != nil {
				return err
			}
			cfg, err := s.load()
			if err != nil {
				return err
			}
			current := a.profileName(cfg)
			rows := [][]string{}
			for _, name := range sortedProfiles(cfg) {
				p := cfg.Profiles[name]
				marker := ""
				if name == current {
					marker = "*"
				}
				base := p.BaseURL
				if base == "" {
					base = kaab.DefaultBaseURL
				}
				rows = append(rows, []string{marker, name, p.Org, strings.Join(p.Modes, ","), p.Storage.String(), base})
			}
			if len(rows) > 0 {
				if err := table(a.env.stdout, []string{"", "PROFILE", "ORG", "KEYS", "STORAGE", "BASE URL"}, rows); err != nil {
					return err
				}
			}
			c, _, err := a.client()
			if err != nil {
				return err
			}
			o, err := c.Org.Get(cmd.Context())
			if err != nil {
				return err
			}
			if a.g.json {
				return printJSON(a.env.stdout, o)
			}
			_, _ = fmt.Fprintf(a.env.stdout, "Authenticated to %s (%s), plan %s, against %s.\n", o.Name, o.ID, o.Plan, c.BaseURL())
			return nil
		},
	}

	cmd.AddCommand(login, logout, status)
	return cmd
}
