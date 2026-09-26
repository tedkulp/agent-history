# Agent History

Gathers coding-agent conversation history from many machines into one place to browse and search.

## Language

**Hub**:
The single central server that stores all collected history and serves the web interface.
_Avoid_: server, backend

**Collector**:
The program installed on each desktop that reads local history from every Source and ships it to the Hub.
_Avoid_: client, agent, daemon

**Machine**:
A desktop running a Collector, identified by a stable ID and labelled by its hostname.
_Avoid_: host, device, client

**Source**:
A supported coding tool whose history is collected: Claude Code, Codex, oh-my-pi, or opencode.
_Avoid_: agent, tool, provider

**Session**:
One conversation in one Source on one Machine.
_Avoid_: agent, chat, thread

**Project**:
The working directory a Session started in, on one Machine. Sessions started in the home directory or a temp directory belong to no Project.
_Avoid_: repo, workspace

**Transcript**:
The ordered messages of a Session, including tool calls, tool results, and usage.

**Raw record**:
The Source's original on-disk data for a Session, kept verbatim alongside the parsed Transcript.

## Relationships

- A **Machine** runs exactly one **Collector**
- A **Collector** reads from zero or more **Sources** installed on its **Machine**
- A **Session** belongs to exactly one **Source** and one **Machine**
- A **Session** has one **Transcript** and one or more **Raw records**
- A **Project** belongs to exactly one **Machine**; the same path on two Machines is two **Projects**
- A **Session** belongs to at most one **Project**, fixed by where it started

## Flagged ambiguities

- "agent" was used for the coding tool, a running session, and the desktop program. Resolved: **Source**, **Session**, and **Collector** respectively.
- "client" could mean the desktop program or the browser. Resolved: **Collector**.
