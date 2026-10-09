# Contributing Guidelines

Guidelines for developing and contributing to this project.

## List of project maintainers

- [Andrii Kislitsyn](https://github.com/andriykislitsyn)
- [Carson Gee](https://github.com/carsongee)

## Running the checks

Run `task lint` and `task test` before you open a pull request. `task --list` shows the other tasks, and [SPEC.md](SPEC.md) describes the sync semantics a change must keep.

Every Go file starts with the Apache 2.0 header in `.licenserc.yaml`. CI checks it with [SkyWalking Eyes](https://github.com/apache/skywalking-eyes). To run it locally, install it with `go install github.com/apache/skywalking-eyes/cmd/license-eye@v0.9.0`, then run `license-eye -c .licenserc.yaml header fix` to add a missing header.

## Opening new issues

- Before opening a new issue check if there are any existing FAQ entries (if one exists), issues or pull requests that match your case
- Open an issue, and make sure to label the issue accordingly - bug, improvement, feature request, etc...
- Be as specific and detailed as possible

## Did you find a bug?

- Do not open up a GitHub issue if the bug is a security vulnerability, instead follow [SECURITY.md](SECURITY.md)
- Ensure the bug was not already reported in the projects Issues section
- Open an issue as described above

## Responding to issues and pull requests

This project is community maintained. Maintainers respond on a best-effort basis, with no response-time commitment.

If your issue or pull request gets no response, you can email oss-community-management@datarobot.com
