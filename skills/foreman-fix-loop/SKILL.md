---
name: foreman-fix-loop
description: Fix failing tests one hypothesis at a time, with the smallest change, and stop after 3 failed attempts with a short failure report. Use when tests fail during implementation.
---

# Fix loop

Tests are failing. Fix them methodically, and know when to stop.

## One attempt

1. Run the tests and read the full output, not only the last lines:
   the first error is often the real one.
2. Form one hypothesis: a single cause that explains the failure. Write
   it down in one sentence before changing anything.
3. Make the smallest change that tests this hypothesis. Change production
   code only: never edit, skip or weaken the tests committed by
   foreman-tdd.
4. Re-run the tests.
   - They pass: stop, you are done.
   - Something else fails: that is new information. Count the attempt and
     start again from step 1.
   - The same failure: the hypothesis was wrong. Revert the change, count
     the attempt and start again from step 1 with a different hypothesis.

## Stop after 3 failed attempts

After 3 attempts that did not make the tests pass, stop. Do not try a
fourth time, do not rewrite large parts of the code to get through, do
not disable tests. Leave the code in the state of the most promising
attempt and write a failure report instead.

## Failure report

```markdown
## Failure report

### Still failing
- `<test name>`: <the error, one line>

### Attempts
1. Hypothesis: ... Change: ... Result: ...
2. ...
3. ...

### Best guess
What you think the real cause is and what a human should look at first.
```

Keep it short: a reader should understand it in under a minute.
