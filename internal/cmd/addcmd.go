package cmd

import (
	"fmt"
	"io/fs"

	"github.com/spf13/cobra"

	"github.com/twpayne/chezmoi/v2/internal/chezmoi"
)

type addCmdConfig struct {
	Encrypt          bool     `json:"encrypt"          mapstructure:"encrypt"          yaml:"encrypt"`
	Secrets          severity `json:"secrets"          mapstructure:"secrets"          yaml:"secrets"`
	TemplateSymlinks bool     `json:"templateSymlinks" mapstructure:"templateSymlinks" yaml:"templateSymlinks"`
	autoTemplate     bool
	create           bool
	exact            bool
	filter           *chezmoi.EntryTypeFilter
	follow           bool
	prompt           bool
	quiet            bool
	recursive        bool
	template         bool
}

func (c *Config) newAddCmd() *cobra.Command {
	addCmd := &cobra.Command{
		Use:     "add targets...",
		Aliases: []string{"manage"},
		Short:   "Add an existing file, directory, or symlink to the source state",
		Long:    mustLongHelp("add"),
		Example: example("add"),
		Args:    cobra.MinimumNArgs(1),
		RunE:    c.makeRunEWithSourceState(c.runAddCmd),
		Annotations: newAnnotations(
			createSourceDirectoryIfNeeded,
			modifiesSourceDirectory,
			persistentStateModeReadWrite,
			requiresWorkingTree,
		),
	}

	flags := addCmd.Flags()
	flags.BoolVarP(
		&c.Add.autoTemplate,
		"autotemplate",
		"a",
		c.Add.autoTemplate,
		"Generate the template when adding files as templates",
	)
	flags.BoolVar(&c.Add.create, "create", c.Add.create, "Add files that should exist, irrespective of their contents")
	flags.BoolVar(&c.Add.Encrypt, "encrypt", c.Add.Encrypt, "Encrypt files")
	flags.BoolVar(&c.Add.exact, "exact", c.Add.exact, "Add directories exactly")
	flags.VarP(c.Add.filter.Exclude, "exclude", "x", "Exclude entry types")
	flags.BoolVarP(&c.Add.follow, "follow", "f", c.Add.follow, "Add symlink targets instead of symlinks")
	flags.VarP(c.Add.filter.Include, "include", "i", "Include entry types")
	flags.BoolVarP(&c.Add.prompt, "prompt", "p", c.Add.prompt, "Prompt before adding each entry")
	flags.BoolVarP(&c.Add.quiet, "quiet", "q", c.Add.quiet, "Suppress warnings")
	flags.BoolVarP(&c.Add.recursive, "recursive", "r", c.Add.recursive, "Recurse into subdirectories")
	flags.Var(&c.Add.Secrets, "secrets", "Scan for secrets when adding unencrypted files")
	flags.BoolVarP(&c.Add.template, "template", "T", c.Add.template, "Add files as templates")
	flags.BoolVar(
		&c.Add.TemplateSymlinks,
		"template-symlinks",
		c.Add.TemplateSymlinks,
		"Add symlinks with target in source or home dirs as templates",
	)

	registerExcludeIncludeFlagCompletionFuncs(addCmd)
	if err := addCmd.RegisterFlagCompletionFunc("secrets", severityFlagCompletionFunc); err != nil {
		panic(err)
	}

	return addCmd
}

func (c *Config) defaultOnIgnoreFunc(targetRelPath chezmoi.RelPath) {
	if !c.Add.quiet {
		c.errorf("warning: ignoring %s\n", targetRelPath)
	}
}

func (c *Config) defaultPreAddFunc(targetRelPath chezmoi.RelPath, fileInfo fs.FileInfo) error {
	// Scan unencrypted files for secrets, if configured.
	if c.Add.Secrets != severityIgnore && fileInfo.Mode().Type() == 0 && !c.Add.Encrypt {
		absPath := c.DestDirAbsPath.Join(targetRelPath)
		content, err := c.destSystem.ReadFile(absPath)
		if err != nil {
			return err
		}
		gitleaksDetector, err := c.getGitleaksDetector()
		if err != nil {
			return err
		}
		findings := gitleaksDetector.DetectBytes(content)
		for _, finding := range findings {
			c.errorf("%s:%d: %s\n", absPath, finding.StartLine+1, finding.Description)
		}
		if !c.force && c.Add.Secrets == severityError && len(findings) > 0 {
			return chezmoi.ExitCodeError(1)
		}
	}

	if !c.Add.prompt {
		return nil
	}

	prompt := fmt.Sprintf("add %s", c.SourceDirAbsPath.Join(targetRelPath))
	for {
		switch choice, err := c.promptChoice(prompt, choicesYesNoAllQuit); {
		case err != nil:
			return err
		case choice == "all":
			c.Add.prompt = false
			return nil
		case choice == "no":
			return fs.SkipDir
		case choice == "quit":
			return chezmoi.ExitCodeError(0)
		case choice == "yes":
			return nil
		default:
			panic(choice + ": unexpected choice")
		}
	}
}

// defaultReplaceFunc prompts the user for confirmation if the adding the entry
// would remove any of the encrypted, private, or template attributes.
func (c *Config) defaultReplaceFunc(
	targetRelPath chezmoi.RelPath,
	newSourceStateEntry, oldSourceStateEntry chezmoi.SourceStateEntry,
) error {
	if c.force {
		return nil
	}

	newFile, newIsFile := newSourceStateEntry.(*chezmoi.SourceStateFile)
	oldFile, oldIsFile := oldSourceStateEntry.(*chezmoi.SourceStateFile)
	if !newIsFile || !oldIsFile {
		return nil
	}

	var removedAttributes []string
	if !newFile.Attr.Encrypted && oldFile.Attr.Encrypted {
		removedAttributes = append(removedAttributes, "encrypted")
	}
	if !newFile.Attr.Private && oldFile.Attr.Private {
		removedAttributes = append(removedAttributes, "private")
	}
	if !newFile.Attr.Template && oldFile.Attr.Template {
		removedAttributes = append(removedAttributes, "template")
	}
	if len(removedAttributes) == 0 {
		return nil
	}
	removedAttributesStr := englishListWithNoun(removedAttributes, "attribute", "")
	prompt := fmt.Sprintf("adding %s would remove %s, continue", targetRelPath, removedAttributesStr)

	for {
		switch choice, err := c.promptChoice(prompt, choicesYesNoAllQuit); {
		case err != nil:
			return err
		case choice == "all":
			c.force = true
			return nil
		case choice == "no":
			return fs.SkipDir
		case choice == "quit":
			return chezmoi.ExitCodeError(0)
		case choice == "yes":
			return nil
		default:
			panic(choice + ": unexpected choice")
		}
	}
}

func (c *Config) runAddCmd(cmd *cobra.Command, args []string, sourceState *chezmoi.SourceState) error {
	destAbsPathInfos, err := c.destAbsPathInfos(sourceState, args, destAbsPathInfosOptions{
		follow:    c.Mode == chezmoi.ModeSymlink || c.Add.follow,
		recursive: c.Add.recursive,
	})
	if err != nil {
		return err
	}

	persistentStateFileAbsPath, err := c.persistentStateFile()
	if err != nil {
		return err
	}

	return sourceState.Add(
		c.sourceSystem,
		c.persistentState,
		c.destSystem,
		destAbsPathInfos,
		&chezmoi.AddOptions{
			AutoTemplate:    c.Add.autoTemplate,
			Create:          c.Add.create,
			Encrypt:         c.Add.Encrypt,
			EncryptedSuffix: c.encryption.EncryptedSuffix(),
			Exact:           c.Add.exact,
			Filter:          c.Add.filter,
			OnIgnoreFunc:    c.defaultOnIgnoreFunc,
			PreAddFunc:      c.defaultPreAddFunc,
			ProtectedAbsPaths: []chezmoi.AbsPath{
				c.CacheDirAbsPath,
				c.WorkingTreeAbsPath,
				c.getConfigFileAbsPath(),
				persistentStateFileAbsPath,
				c.sourceDirAbsPath,
			},
			ReplaceFunc:      c.defaultReplaceFunc,
			Template:         c.Add.template,
			TemplateSymlinks: c.Add.TemplateSymlinks,
		},
	)
}
