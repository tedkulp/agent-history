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

**Child Session**:
A Session spawned by a Tool call in another Session, reached through that parent rather than listed on its own.
_Avoid_: sub-agent, sidechain

**Transcript**:
The ordered Messages of a Session, following its latest branch only.

**Message**:
One turn in a Transcript, by the user, the assistant, or a tool, made of ordered Parts.
_Avoid_: event, entry

**Part**:
One typed piece of a Message: text, thinking, Tool call, image, attachment, marker, or unknown.
_Avoid_: block, item

**Tool call**:
A tool request together with its result, treated as one unit.

**Raw record**:
The Source's original on-disk data for a Session, kept verbatim alongside the parsed Transcript: one Source file, or one Session's rows for a database-backed Source. When its content is rewritten rather than appended to, the earlier content is kept as a superseded version.

**Record key**:
The stable name of a Raw record within its Source on one Machine. It stays the same when the Source compresses or archives the underlying file.
_Avoid_: path, filename

## Relationships

- A **Machine** runs exactly one **Collector**
- A **Collector** reads from zero or more **Sources** installed on its **Machine**
- A **Session** belongs to exactly one **Source** and one **Machine**
- A **Session** has one **Transcript** and one or more **Raw records**
- A **Raw record** is identified by (**Machine**, **Source**, **Record key**)
- A **Project** belongs to exactly one **Machine**; the same path on two Machines is two **Projects**
- A **Session** belongs to at most one **Project**, fixed by where it started
- A **Child Session** has exactly one parent **Session** and is spawned by one **Tool call** in it
- A **Transcript** has one or more **Messages**; a **Message** has one or more **Parts**

## Flagged ambiguities

- "agent" was used for the coding tool, a running session, and the desktop program. Resolved: **Source**, **Session**, and **Collector** respectively.
- "client" could mean the desktop program or the browser. Resolved: **Collector**.
- Sources call sub-agent runs "sidechains", "sub-agents", or child sessions. Resolved: **Child Session**.
