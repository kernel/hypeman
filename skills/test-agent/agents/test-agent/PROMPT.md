# This file is managed by human operators and should not be modified by an agent.

Your task is to improve the testing for this repository by eliminating flakes, or by speeding up tests, or by removing longer-running test redundancy, or by improving the speed of the core.

In order of priority:

- Tests run without flakes
- Tests run quickly as a whole when there is no caching
- Any individual test runs quickly
- Minimal test redundancy for slow tests
- Tests run with minimal or no remote dependencies

Approach:
- Run the full testing like it runs in CI twice without caching, remember the duration of the test run.
- If there are flakes, focus on the flakes to eliminate all test flakiness. We will only address flakiness in our change.
- Only if there are no flakes, then we will next focus on test redundancy.
- Analyze tests that take longer than 2 seconds for redundancy in what they are testing. If two tests are truly redundant or there’s opportunity to combine slow tests in a way to make a large speed improvement, then do it.
- After assessing testing redundancy, then we will focus on making the longest running individual test run faster
- If the individual longest test is faster than 5 seconds and we already addressed all redundancy, then we can exit without improving anything. Just exit in success without changing anything in that case.
- If we are working on the individual longest test instead of flakiness, analyze what time is spent on in the longest test. Can we improve the test or the core itself to be faster?
- Does the test truly need to take that long? In general, any test over 10 seconds is suspect.

How to work on fixes:
- Use the tightest possible test cycle to validate specific changes or ideas
- Only after validating in a tight test loop retry the full suite without caching

Rules:
- Never add sleeps in code or testing, exception for short polling only when really needed
- Never add new skips in testing
- Do not decrease parallelism to address flakiness, instead address the source of the flakiness
- Do not make a commit with over 500 lines of code added in the non-test files
- Do not make a commit with over 2k lines of code added total.
- Never remove end-user facing functionality
- Never reduce the scope of what is being tested, but it is OK to remove or consolidate redundant, slow tests

Guidelines:
- Address flakiness with simple changes in the core code when flakes may be applicable in real operation
- Address flakiness with tweaks in the tests when flakiness is unique to testing environment
- Disable caching when running manually to test for flakes
- Leave caching enabled for CI runs
- Take notes that help avoid redundant discovery work in your testing improvements in agents/test-agent/NOTES.md
- This prompt is located in agents/test-agent/PROMPT.md, don't modify that
- Seek to minimize or remove remote dependencies, as these may be sources of flakiness
- Assume you have a lot of resources available in the testing environment

Required check before commits:
- You must ensure that all tests run three times in a row without caching with no flakes before committing

After committing, don’t push up anything. Just stop and summarize what you did to make concrete improvements. Thank you.
