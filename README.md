# orchestra-tennant

**Orchestra** is an orchestrator for agent-driven software development. It lets you create new projects or connect existing ones in a couple of clicks, integrates with the repositories you already use — GitLab today, GitHub coming — and takes a task from analysis to a ready branch through a pipeline of AI agents: planning, review of the plan, implementation, tests, code review. You watch the progress, answer the agents' questions and send corrections in chat; Orchestra does the rest.

Orchestra never touches your code itself. The work happens on **your machines**: a laptop, a workstation, a build server — any device you connect. The code, the git credentials and the AI subscription stay where they are; the orchestrator only receives the progress, the artifacts and the diff.

**orchestra-tennant** is the piece that runs on such a machine. Install it on every device where your projects live, pair it with the orchestrator once, and the device shows up in Orchestra as a place where tasks can run.

## Requirements

- macOS or Linux
- [`claude` CLI](https://docs.anthropic.com/claude-code) with a completed login (`claude login`)
- `git`

Go is **not** required — ready-to-run binaries are published in [Releases](https://github.com/realkasparov/orchestra-tennant/releases).

## Install

One command downloads the latest release, verifies its checksum and puts the binary into `~/.local/bin`:

```bash
curl -fsSL https://github.com/realkasparov/orchestra-tennant/releases/latest/download/install.sh | sh
```

Prefer to do it by hand? Download the archive for your system from the [release page](https://github.com/realkasparov/orchestra-tennant/releases/latest), unpack it and put `orchestra-tennant` anywhere on your `PATH`:

```bash
tar -xzf orchestra-tennant_darwin_arm64.tar.gz
mv orchestra-tennant ~/.local/bin/
```

Check:

```bash
orchestra-tennant version
```

> macOS may quarantine a binary downloaded with a browser. Release it with
> `xattr -d com.apple.quarantine ~/.local/bin/orchestra-tennant`.

## Connect to Orchestra

1. In Orchestra open **Devices → Add device**, name the machine and get a pairing key. The key is shown once and is valid for 15 minutes; the panel also shows a ready-made setup command.

2. Run that command on the machine:

   ```bash
   orchestra-tennant setup -orchestrator http://127.0.0.1:8765 -pair-key XXXXX-XXXXX-XXXXX-XXXXX
   ```

   Setup asks a few questions — which models to use, how many tasks to run at once, where to keep projects — checks that everything works, and offers to install itself as a background service that starts on boot. You can also run it in a terminal instead:

   ```bash
   orchestra-tennant run
   ```

A few seconds later the device card in Orchestra turns **online**. From now on you can create projects on this machine and run tasks in them.

## Commands

```
orchestra-tennant setup                 configure (re-running does not require a new key)
orchestra-tennant run                   run in the terminal
orchestra-tennant status                configuration, service, connection to Orchestra
orchestra-tennant logs [-n 50] [-f]     show the log; -f follows it
orchestra-tennant service install       install the background service
orchestra-tennant service uninstall     remove it
orchestra-tennant update [-check]       update to the latest release
orchestra-tennant version
```

## Update

```bash
orchestra-tennant update
```

The command downloads the latest release, verifies it, replaces the binary and restarts the background service if one is installed. `update -check` only reports whether a newer version exists.

## If something goes wrong

```bash
orchestra-tennant status
orchestra-tennant logs -n 100
```

- **device key not accepted** — the device was revoked or its key was reissued in Orchestra: get a new pairing key on the device card and run `setup` again.
- **models do not respond** — `claude` is not installed or not logged in: run `claude login`.
- **cannot reach the orchestrator** — check the address and that Orchestra is running.

## License

[Apache License 2.0](LICENSE)
