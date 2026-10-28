---
name: react-conventions
description: React and TypeScript coding rules and the standard commands to format, lint, type-check and test a React project. Use when writing or reviewing React code.
---

# React conventions

Follow AGENTS.md and the style already in the repository first. These
rules apply where they are silent.

## Code

- TypeScript in strict mode. No `any`; use `unknown` and narrow it.
- Function components and hooks only. One component per file, named like
  the file.
- Props are typed with an explicit type. Keep components small: move
  logic into custom hooks (`useSomething`) or plain functions.
- Follow the rules of hooks; never silence `react-hooks/exhaustive-deps`
  without a comment explaining why.
- State lives as low as possible. Derive values instead of storing them.
- Effects are for synchronising with the outside world, not for
  computing values.
- Lists have stable `key`s from the data, never the array index.
- Accessible by default: semantic elements, labels on inputs, `alt` on
  images, keyboard support for anything clickable.
- Never use `dangerouslySetInnerHTML` with data you did not produce.
- Use the project's existing libraries for data fetching, routing and
  styling. Do not add a new one without saying why in the PR.

## Tests

- Test behaviour, not implementation: React Testing Library, queries by
  role and label, `user-event` for interactions.
- Mock the network at the boundary (for example with MSW), not
  components or hooks.
- Every bug fix comes with a test that fails without it.

## Commands

Use the scripts from `package.json` when they exist:

```sh
npm ci                   # clean install from the lock file
npx prettier --check .
npm run lint             # ESLint
npx tsc --noEmit         # type-check
npm test                 # unit and component tests
npm run build
```

Run all of them before committing. A change is not done while any of
them fails.
