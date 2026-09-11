# Security policy

District Scheduler is a fork of [Calnode](https://github.com/Calnode/calnode), run by
Distronode Corporation.

## Supported version

The `district` branch, as deployed. There are no release lines and no backports: a fix
lands on `district` and reaches the fleet on the next image build. If you are running an
older image digest, the fix for a reported issue is an upgrade.

## Reporting a vulnerability

Please report privately, through GitHub's private vulnerability reporting on **this**
repository: open the **Security** tab and use **Report a vulnerability**. That opens a
private thread visible only to you and the maintainers.

If the issue is in upstream Calnode code rather than in this fork's changes, please also
report it to Calnode, so that every other deployment gets the fix and not just ours.

Please do not open a public issue, a pull request, or a discussion for a security report.
A public issue is a disclosure, and it is one made before there is anything for people to
upgrade to.

## What to include

Enough to reproduce it. Usually that is:

- what the problem is, and what an attacker gets out of it;
- the commit or image digest you tested;
- how you are running it, and anything non-default in your configuration;
- the steps, request, or proof-of-concept that triggers it;
- what you expected to happen instead.

If you are not sure whether something is a vulnerability, report it anyway and say so.

## What to expect

- **An acknowledgement within a few days.** If you have not heard anything after a week,
  please post a follow-up on the same private thread in case it was missed.
- **Then either a fix or an explanation.** If we agree it is a vulnerability, we will
  tell you roughly when a fix will land and let you know when it ships. If we do not
  think it is one, we will say why rather than leaving the report open.
- **Credit, if you want it.** Tell us the name or handle to use, or tell us you would
  rather stay anonymous. Either is fine.

Please give us a reasonable chance to ship a fix before publishing the details.
