package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/flemzord/skillloop/internal/domain"
	"github.com/flemzord/skillloop/internal/improvement"
	"github.com/flemzord/skillloop/internal/pipeline"
)

const cliActor = "cli"

func newProposalCommand(options *rootOptions) *cobra.Command {
	command := &cobra.Command{Use: "proposal", Short: "Manage skill improvement proposals"}
	command.AddCommand(
		newProposalListCommand(options),
		newProposalCreateCommand(options),
		newProposalShowCommand(options),
		newProposalEvaluateCommand(options),
		newProposalApproveCommand(options),
		newProposalRejectCommand(options),
	)
	return command
}

func newProposalListCommand(options *rootOptions) *cobra.Command {
	jsonOutput := false
	status := ""
	command := &cobra.Command{
		Use:   "list",
		Short: "List proposals",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			proposals, err := state.store.ListProposals(command.Context(), domain.ProposalStatus(status))
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(proposals)
			}
			for _, proposal := range proposals {
				if _, err := fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\t%s\n", proposal.ID, proposal.Status, proposal.SkillID, proposal.ClusterID); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.Flags().StringVar(&status, "status", "", "filter by proposal status")
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return command
}

func newProposalCreateCommand(options *rootOptions) *cobra.Command {
	jsonOutput := false
	command := &cobra.Command{
		Use:   "create <cluster-id>",
		Short: "Create a proposal for an eligible cluster",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			proposal, created, err := pipeline.New(state.config, state.store).Create(command.Context(), args[0], cliActor)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(proposalCreateOutput{Created: created, Proposal: proposal})
			}
			action := "existing"
			if created {
				action = "created"
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "%s\t%s\t%s\n", action, proposal.ID, proposal.Status)
			return err
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return command
}

func newProposalShowCommand(options *rootOptions) *cobra.Command {
	jsonOutput := false
	command := &cobra.Command{
		Use:   "show <id>",
		Short: "Show a proposal and its evaluation history",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			view, err := pipeline.New(state.config, state.store).Show(command.Context(), args[0])
			if err != nil {
				return err
			}
			if jsonOutput {
				encoder := json.NewEncoder(command.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(view)
			}
			return writeProposalView(command.OutOrStdout(), view)
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return command
}

func newProposalEvaluateCommand(options *rootOptions) *cobra.Command {
	jsonOutput := false
	command := &cobra.Command{
		Use:   "evaluate <id>",
		Short: "Evaluate a proposal candidate",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			proposal, evaluation, err := pipeline.New(state.config, state.store).Evaluate(command.Context(), args[0], cliActor)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(proposalEvaluationOutput{Proposal: proposal, Evaluation: evaluation})
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "evaluated\t%s\tpassed=%t\tbaseline=%.2f\tcandidate=%.2f\n", proposal.ID, evaluation.Passed, proposal.BaselineScore, proposal.CandidateScore)
			return err
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return command
}

func newProposalApproveCommand(options *rootOptions) *cobra.Command {
	jsonOutput := false
	command := &cobra.Command{
		Use:   "approve <id>",
		Short: "Approve and promote an evaluated proposal",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			promotion, err := pipeline.New(state.config, state.store).Approve(command.Context(), args[0], cliActor)
			if err != nil {
				return err
			}
			if jsonOutput {
				return json.NewEncoder(command.OutOrStdout()).Encode(promotion)
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "approved\t%s\t%s\t%s\n", promotion.ProposalID, promotion.PreviousCommit, promotion.PromotedCommit)
			return err
		},
	}
	command.Flags().BoolVar(&jsonOutput, "json", false, "emit JSON")
	return command
}

func newProposalRejectCommand(options *rootOptions) *cobra.Command {
	reason := "rejected by user"
	command := &cobra.Command{
		Use:   "reject <id>",
		Short: "Reject a proposal",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			state, err := openRuntime(command.Context(), options.configPath)
			if err != nil {
				return err
			}
			defer state.close()
			if err := pipeline.New(state.config, state.store).Reject(command.Context(), args[0], cliActor, reason); err != nil {
				return err
			}
			_, err = fmt.Fprintf(command.OutOrStdout(), "rejected\t%s\t%s\n", args[0], reason)
			return err
		},
	}
	command.Flags().StringVar(&reason, "reason", reason, "reason for rejecting the proposal")
	return command
}

type proposalCreateOutput struct {
	Created  bool            `json:"created"`
	Proposal domain.Proposal `json:"proposal"`
}

type proposalEvaluationOutput struct {
	Proposal   domain.Proposal        `json:"proposal"`
	Evaluation improvement.Evaluation `json:"evaluation"`
}

func writeProposalView(writer io.Writer, view pipeline.ProposalView) error {
	if _, err := fmt.Fprintf(writer, "proposal\t%s\t%s\t%s\t%s\n", terminalSafe(view.Proposal.ID), view.Proposal.Status, terminalSafe(view.Proposal.SkillID), terminalSafe(view.Proposal.ClusterID)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "commits\tbase=%s\tcandidate=%s\n", terminalSafe(view.Proposal.BaseCommit), terminalSafe(view.Proposal.CandidateCommit)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "scores\tbaseline=%.2f\tcandidate=%.2f\n", view.Proposal.BaselineScore, view.Proposal.CandidateScore); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "requires_human_approval\t%t\n", view.RequiresHumanApproval); err != nil {
		return err
	}
	for _, evaluation := range view.Evaluations {
		if _, err := fmt.Fprintf(writer, "evaluation\t%s\tpassed=%t\tscore=%.2f\n", terminalSafe(string(evaluation.Variant)), evaluation.Passed, evaluation.Score); err != nil {
			return err
		}
	}
	for _, audit := range view.Audit {
		if _, err := fmt.Fprintf(writer, "audit\t%s\t%s\t%s\n", terminalSafe(audit.Action), terminalSafe(audit.Actor), terminalSafe(audit.Details)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(writer, "diff_begin"); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(writer, terminalSafe(view.Diff)); err != nil {
		return err
	}
	_, err := fmt.Fprintln(writer, "diff_end")
	return err
}

// terminalSafe preserves the line and column structure needed for human diff
// review while rendering terminal control sequences as inert visible text.
// Machine-readable JSON uses the original values and is intentionally exact.
func terminalSafe(value string) string {
	var output strings.Builder
	for len(value) > 0 {
		if value[0] < utf8.RuneSelf {
			character := value[0]
			value = value[1:]
			switch {
			case character == '\n' || character == '\t' || character >= 0x20 && character <= 0x7e:
				output.WriteByte(character)
			default:
				_, _ = fmt.Fprintf(&output, `\x%02x`, character)
			}
			continue
		}
		character, size := utf8.DecodeRuneInString(value)
		if character == utf8.RuneError && size == 1 {
			_, _ = fmt.Fprintf(&output, `\x%02x`, value[0])
			value = value[1:]
			continue
		}
		value = value[size:]
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			_, _ = fmt.Fprintf(&output, `\u%04x`, character)
			continue
		}
		output.WriteRune(character)
	}
	return output.String()
}
