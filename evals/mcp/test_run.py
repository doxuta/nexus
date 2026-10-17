#!/usr/bin/env python3
"""The eval's pass rule, checked on transcripts instead of live runs: python3 evals/mcp/test_run.py

testdata/recorded-task-5.jsonl is task 5 of the 2026-10-17 run (README), trimmed: the recording machine's init
fields (cwd, memory paths, plugins, skills, slash commands) and the opaque thinking blocks are dropped; every other
event is as recorded. testdata/failed-connection.jsonl is synthetic: the nexus server failed to connect, the agent
answered from nothing and the session still exited 0. That is how task 5 could pass without reaching the server.
Standard library only.
"""
import os
import unittest

import run

HERE = os.path.dirname(os.path.abspath(__file__))
TASK_5 = next(t for t in run.TASKS if t[0] == 5)


def events(name):
    with open(os.path.join(HERE, "testdata", name)) as f:
        return run.parse_events(f.read())


def accepting(task):
    """The task with a grader that accepts any proposals, so only the rest of the pass rule decides."""
    return (task[0], task[1], lambda ds: True, task[3])


class PassRule(unittest.TestCase):
    def test_the_recorded_task_5_passes(self):
        ev = events("recorded-task-5.jsonl")
        self.assertEqual(run.tool_calls(ev), ["nexus_list_entities", "nexus_list_roles", "nexus_explain_access"])
        self.assertTrue(run.passes(TASK_5, ev, [], 0))

    def test_a_session_that_never_reached_the_server_fails_every_task(self):
        ev = events("failed-connection.jsonl")
        self.assertEqual(run.tool_calls(ev), [])
        self.assertFalse(run.passes(TASK_5, ev, [], 0), "task 5 passed although the server was never reached")
        for t in run.TASKS:
            with self.subTest(task=t[0]):
                self.assertFalse(run.passes(accepting(t), ev, [], 0))

    def test_either_signal_is_enough(self):
        ev = events("recorded-task-5.jsonl")
        init = [e for e in ev if e.get("subtype") == "init"]
        rest = [e for e in ev if e.get("subtype") != "init"]
        self.assertTrue(run.passes(accepting(TASK_5), rest, [], 0), "a nexus_ tool call, no init event")
        self.assertTrue(run.passes(accepting(TASK_5), init, [], 0), "the nexus server connected, no tool call")

    def test_a_failed_session_fails(self):
        self.assertFalse(run.passes(TASK_5, events("recorded-task-5.jsonl"), [], 1))


if __name__ == "__main__":
    unittest.main()
