# Contributing to DevZero ZXporter

Thank you for your interest in contributing to **DevZero ZXporter**!

We welcome contributions of all kinds—whether it's a bug fix, a new feature, documentation improvements, or anything else that can help make the project better.

By participating in this project, you agree to abide by our [Code of Conduct](.github/CODE_OF_CONDUCT.md).

---

## Table of Contents

- [Reporting Issues](#reporting-issues)
- [Pull Request Guidelines](#pull-request-guidelines)
- [Commit Message Guidelines](#commit-message-guidelines)
- [Testing](#testing)
- [Linting](#linting)
- [Documentation](#documentation)
- [Component-Specific Guidelines](#component-specific-guidelines)
- [Backporting Changes](#backporting-changes)
- [Questions and Communication](#questions-and-communication)

---

## Reporting Issues

If you discover a bug, have a feature request, or need to provide feedback, please open a GitHub issue. When reporting an issue, try to include the following:

- A clear title and detailed description.
- Steps to reproduce the issue.
- Expected and actual behavior.
- Screenshots or logs, if applicable.
- Any relevant labels to help us categorize the issue.

---

## Pull Request Guidelines

When you're ready to contribute code:

1. **Fork and Clone**  
   Fork the repository and clone it to your local machine.
2. **Create a Branch**  
   Create a feature branch from the appropriate base branch (typically `main`).
3. **Make Changes**  
   Implement your changes, ensuring they adhere to the project's coding standards.
4. **Run Tests**  
   Verify that all tests pass locally. Refer to the testing section below for more details.
5. **Update Documentation**  
   Update or add documentation as needed in the corresponding directories.
6. **Commit and Push**  
   Write clear commit messages that reference the related issue (e.g., "fix: Description of fix").
7. **Open a Pull Request**  
   Submit a pull request (PR) with a detailed description of your changes and link any related issues.

---

## Commit Message Guidelines

- Write clear and descriptive commit messages.
- Reference GitHub issues using the format `fix: Message`.
- Follow conventional commit formats if possible, keeping messages brief yet descriptive.

---

## Changelog Guidelines

We use automated changelog generation based on PR labels. To ensure your changes are properly documented:

### PR Labels for Changelog

When creating a PR, apply one of the following labels to categorize your change:

| Label | Use When |
|-------|----------|
| `changelog:added` | Adding new features or capabilities |
| `changelog:changed` | Modifying existing functionality |
| `changelog:fixed` | Fixing bugs or issues |
| `changelog:removed` | Removing features or capabilities |
| `changelog:security` | Addressing security vulnerabilities |
| `changelog:skip` | Changes that should not appear in changelog (CI, docs, chores) |

### Breaking Changes

If your PR introduces a breaking change:
1. Add the `changelog:breaking` label
2. Clearly document the breaking change in your PR description
3. Include migration instructions if applicable

### Automatic Labeling

Labels are automatically suggested based on PR titles:
- Titles containing "fix", "bug" → `changelog:fixed`
- Titles containing "add", "feat", "new" → `changelog:added`
- Titles containing "update", "improve", "enhance" → `changelog:changed`
- Titles starting with "chore", "ci", "docs" → `changelog:skip`

You can override automatic labels by manually applying the correct label.

---

## Testing

Before submitting a PR, ensure that your changes pass all tests.

We recommend running tests locally to catch issues before opening a PR.

---

## Linting

We use [golangci-lint](https://golangci-lint.run/) to enforce code quality standards. The linter runs automatically on all pull requests and will block merging if issues are found.

### Running the Linter Locally

```bash
# Run linting (will fail on errors)
make lint

# Run linting with auto-fix for fixable issues
make lint-fix
```

### Configuration

Linting rules are configured in `.golangci.yml` at the repository root. The configuration enables these linters:

- **errcheck** - Check for unchecked errors
- **govet** - Report suspicious constructs
- **staticcheck** - Advanced static analysis
- **gofmt/goimports** - Code formatting
- **misspell** - Spelling errors in comments
- **revive** - Extensible linter with many rules
- **unused** - Find unused code
- And more (see `.golangci.yml` for the full list)

### Best Practices

- Run `make lint` before committing to catch issues early
- Use `make lint-fix` to automatically fix formatting issues
- If a lint rule seems incorrect for your use case, discuss it in the PR rather than disabling it

---

## Documentation

Contributions to documentation are highly valued. If you find any documentation gaps or errors:

- Open a PR with your updates.
- Ensure that documentation is clear and consistent with the project’s style.
- For component-specific documentation, please refer to the corresponding README in each directory.

---

<!-- todo -->
<!-- ## Component-Specific Guidelines

**DevZero DevZero ZXporter** is composed of several components. Each has its own nuances:

- **Compose:**
  - Builds Docker Compose version of Dakr ZXporter
  - See [compose/README.md](./compose/README.md) for more details.

If your contribution touches more than one component, please ensure your changes are tested and documented for each area.

--- -->

## Backporting Changes

For changes that need to be applied to previous releases:

- Clearly mention in your PR description which release branches should receive the update.
- Follow any additional guidelines provided by the maintainers for backporting.

---

## Questions and Communication

If you have any questions or need help getting started:

- **Open a GitHub Issue:** Describe your query so we can assist you.
- **Email Us:** Reach out at [support@devzero.com](mailto:support@devzero.com).

For more context about the project, please refer to our [README](./README.md).

---

Thank you for contributing to **DevZero DevZero ZXporter**! Every contribution, no matter how small, is appreciated. We look forward to collaborating with you.

Happy Contributing!
