# API version compatibility

This file exists to make it easy to answer the following questions:

1. Which protocol versions does each component support?
2. Which releases of a component support a given protocol version?

The table below should provide the necessary information. For each release, it gives the range of
supported protocol versions by each component. The topmost line - "Current" - refers to the latest
commit in this repository, possibly unreleased.

## agent<->informant protocol

| Release | autoscaler-agent | VM informant |
|---------|------------------|--------------|
| _Current_ | v1.0 - v1.1 | v1.1 only |
| v0.1.7 | v1.0 - v1.1 | v1.1 only |
| v0.1.6 | v1.0 - v1.1 | v1.1 only |
| v0.1.5 | v1.0 - v1.1 | v1.1 only |
| v0.1.4 | **v1.0 - v1.1** | **v1.1** only |
| v0.1.3 | v1.0 only | v1.0 only |
| 0.1.2 | v1.0 only | v1.0 only |
| 0.1.1 | v1.0 only | v1.0 only |
| 0.1.0 | **v1.0** only | **v1.0** only |

## agent<->scheduler plugin protocol

Note: v0.1.6 and below did not have a versioned protocol between the agent and scheduler plugin.

| Release | autoscaler-agent | Scheduler plugin |
|---------|------------------|------------------|
| _Current_ | **v1.0** only | **v1.0** only |
| v0.1.6 | none | none |
| v0.1.5 | none | none |
| v0.1.4 | none | none |
| v0.1.3 | none | none |
| 0.1.2 | none | none |
| 0.1.1 | none | none |
| 0.1.0 | none | none |
