# ElasticClaw Security Policy

ElasticClaw is a self-hosted platform that manages access to source code, issue trackers, and cloud compute. Security is a top priority.

## Reporting a vulnerability

If you discover a security vulnerability in ElasticClaw, please report it privately rather than opening a public issue.

Email the Project Lead at:

- **marc@replicated.com**

Please include the following in your report:

- A clear description of the vulnerability.
- Steps to reproduce, or a proof of concept if available.
- The affected versions, if known.
- The impact and potential severity.
- Whether you have already disclosed the issue to anyone else.

We will acknowledge receipt of your report within 72 hours, and will work with you to understand and resolve the issue.

## Supported versions

Because ElasticClaw is self-hosted and moving quickly, we generally recommend running the latest release. Security fixes are applied to the most recent release and, when practical, backported to the previous release.

| Version range | Support status |
|---------------|--------------|
| Latest release | Security fixes and patches |
| Previous release | Security fixes when practical |
| Older releases | No longer supported |

## Disclosure policy

We follow a coordinated disclosure process:

1. We confirm the vulnerability and assess severity.
2. We develop and test a fix.
3. We release a patched version and publish a security advisory.
4. We publicly disclose the issue after users have had a reasonable time to upgrade, typically 7–14 days after the fix is released.

We ask that reporters do not publicly disclose the vulnerability until we have released a fix and announced it, unless otherwise agreed in writing.

## Security-related code and settings

If you are operating ElasticClaw in production, please review the following practices:

- Keep ElasticClaw Server updated to the latest release.
- Restrict network access to the server’s admin and API ports.
- Use strong, randomly generated tokens for API and agent authentication.
- Store provider credentials and issue-tracker tokens as secrets, never in plain text in version control.
- Review workspace and workflow permissions before granting broad repository or infrastructure access.
- Enable and review audit logs and agent output for your environment.

## Acknowledgments

We credit security researchers who responsibly disclose vulnerabilities. If you would like to be credited, please let us know when you report the issue.

## Questions

If you have questions about this policy or a specific security concern, contact the Project Lead at `marc@replicated.com`.
