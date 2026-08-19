# Security policy

SkillLoop processes local agent transcripts and can create candidate changes in Git worktrees. Security, privacy, and strict control of side effects are part of its core behavior.

## Supported versions

| Version | Supported |
| --- | --- |
| 0.1.x | Yes |
| Earlier development snapshots | No |

Only the latest patch release in a supported series receives security fixes.

## Reporting a vulnerability

Please report vulnerabilities privately through [GitHub private vulnerability reporting](https://github.com/flemzord/skillloop/security/advisories/new). Do not include sensitive details in a public issue, pull request, discussion, or transcript fixture.

Include, when possible:

- the affected version and operating system;
- the required configuration and threat model;
- minimal reproduction steps;
- the impact and whether secrets or transcript data may have been exposed;
- any suggested mitigation.

You should receive an acknowledgement within seven days and a status update within fourteen days. These are response targets rather than service-level guarantees. Please allow time for a fix and coordinated disclosure before publishing details.

## Security-sensitive areas

Reports are especially useful when they involve:

- transcript, prompt, credential, or personal-data disclosure;
- unsafe path handling, symlink traversal, or writes outside owned skill repositories;
- command or prompt injection leading to unintended local execution;
- Git worktree isolation, promotion, rollback, or signature bypasses;
- permission or security changes promoted without explicit approval;
- hooks that block or materially slow Codex or Claude Code;
- recursive capture of SkillLoop's own sessions;
- retention or redaction behavior that differs from the documented policy.

## Safe testing

Use repositories, transcripts, and credentials that you own or are authorized to test. Do not access other people's data, disrupt services, or retain sensitive material longer than necessary to demonstrate the issue.
