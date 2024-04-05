# API version compatibility

This file exists to make it easy to answer the following questions:

1. Which protocol versions does each component support?
2. Which releases of a component support a given protocol version?

The table below should provide the necessary information. For each release, it gives the range of
supported protocol versions by each component. The topmost line - "Current" - refers to the latest
commit in this repository, possibly unreleased.

## agent<->monitor protocol

Note: For v0.17.0 and below, the autoscaler-agent additionally had support for the vm-informant by
first checking if the /register endpoint returned a 404.

| Release | autoscaler-agent | VM monitor |
|---------|------------------|------------|
| _Current_ | v1.0 only | v1.0 only |
| v0.27.0 | v1.0 only | v1.0 only |
| v0.26.0 | v1.0 only | v1.0 only |
| v0.25.0 | v1.0 only | v1.0 only |
| v0.24.0 | v1.0 only | v1.0 only |
| v0.23.0 | v1.0 only | v1.0 only |
| v0.22.0 | v1.0 only | v1.0 only |
| v0.21.0 | v1.0 only | v1.0 only |
| v0.20.0 | v1.0 only | v1.0 only |
| v0.19.0 | v1.0 only | v1.0 only |
| v0.18.0 | v1.0 only | v1.0 only |
| v0.17.0 | v1.0 only | v1.0 only |
| v0.16.0 | v1.0 only | v1.0 only |
| v0.15.0 | **v1.0** only | **v1.0** only |

## agent<->scheduler plugin protocol

Note: Components v0.1.7 and below did not have a versioned protocol between the agent and scheduler
plugin. We've marked those as protocol version v0.0. Scheduler plugin v0.1.7 implicitly supports
v1.0 because the only change from v0.0 to v1.0 is having the scheduler plugin check the version
number.

| Release | autoscaler-agent | Scheduler plugin |
|---------|------------------|------------------|
| _Current_ | v4.0 only | v3.0-v4.0 |
| v0.27.0 | v4.0 only | v3.0-v4.0 |
| v0.26.0 | v4.0 only | **v3.0-v4.0** |
| v0.25.0 | v4.0 only | v1.0-v4.0 |
| v0.24.0 | v4.0 only | v1.0-v4.0 |
| v0.23.0 | **v4.0 only** | **v1.0-v4.0** |
| v0.22.0 | **v3.0 only** | **v1.0-v3.0** |
| v0.21.0 | v2.1 only | v1.0-v2.1 |
| v0.20.0 | **v2.1 only** | **v1.0-v2.1** |
| v0.19.0 | v2.0 only | v1.0-v2.0 |
| v0.18.0 | v2.0 only | v1.0-v2.0 |
| v0.17.0 | v2.0 only | v1.0-v2.0 |
| v0.16.0 | v2.0 only | v1.0-v2.0 |
| v0.15.0 | v2.0 only | v1.0-v2.0 |
| v0.14.2 | v2.0 only | v1.0-v2.0 |
| v0.14.1 | v2.0 only | v1.0-v2.0 |
| v0.14.0 | v2.0 only | v1.0-v2.0 |
| v0.13.3 | v2.0 only | v1.0-v2.0 |
| v0.13.2 | v2.0 only | v1.0-v2.0 |
| v0.13.1 | v2.0 only | v1.0-v2.0 |
| v0.13.0 | v2.0 only | v1.0-v2.0 |
| v0.12.2 | v2.0 only | v1.0-v2.0 |
| v0.12.1 | v2.0 only | v1.0-v2.0 |
| v0.12.0 | v2.0 only | v1.0-v2.0 |
| v0.11.0 | v2.0 only | v1.0-v2.0 |
| v0.10.0 | v2.0 only | v1.0-v2.0 |
| v0.9.0 | v2.0 only | v1.0-v2.0 |
| v0.8.0 | v2.0 only | v1.0-v2.0 |
| v0.7.2 | v2.0 only | v1.0-v2.0 |
| v0.7.1 | v2.0 only | v1.0-v2.0 |
| v0.7.0 | **v2.0** only | **v1.0-v2.0** |
| v0.6.0 | v1.1 only | v1.0-v1.1 |
| v0.5.2 | v1.1 only | v1.0-v1.1 |
| v0.5.1 | v1.1 only | v1.0-v1.1 |
| v0.5.0 | v1.1 only | v1.0-v1.1 |
| v0.1.17 | v1.1 only | v1.0-v1.1 |
| v0.1.16 | v1.1 only | v1.0-v1.1 |
| v0.1.15 | v1.1 only | v1.0-v1.1 |
| v0.1.14 | v1.1 only | v1.0-v1.1 |
| v0.1.13 | v1.1 only | v1.0-v1.1 |
| v0.1.12 | v1.1 only | v1.0-v1.1 |
| v0.1.11 | v1.1 only | v1.0-v1.1 |
| v0.1.10 | v1.1 only | v1.0-v1.1 |
| v0.1.9 | **v1.1** only | **v1.0-v1.1** |
| v0.1.8 | **v1.0** only | **v1.0** only |
| v0.1.7 | v0.0 only | **v0.0-v1.0** |
| v0.1.6 | v0.0 only | v0.0 only |
| v0.1.5 | v0.0 only | v0.0 only |
| v0.1.4 | v0.0 only | v0.0 only |
| v0.1.3 | v0.0 only | v0.0 only |
| 0.1.2 | v0.0 only | v0.0 only |
| 0.1.1 | v0.0 only | v0.0 only |
| 0.1.0 | **v0.0** only | **v0.0** only |

## controller<->runner protocol

Note: Components v0.6.0 and below did not have a versioned protocol between the controller and the runner.
| Release | controller | runner |
|---------|------------|--------|
| _Current_ | 1 | 1 |
| v0.27.0 | 0 - 1 | 1 |
| v0.26.0 | 0 - 1 | 1 |
| v0.25.0 | 0 - 1 | 1 |
| v0.24.0 | 0 - 1 | 1 |
| v0.23.0 | 0 - 1 | 1 |
| v0.22.0 | 0 - 1 | 1 |
| v0.21.0 | 0 - 1 | 1 |
| v0.20.0 | 0 - 1 | 1 |
| v0.19.0 | 0 - 1 | 1 |
| v0.18.0 | 0 - 1 | 1 |
| v0.17.0 | 0 - 1 | 1 |
| v0.16.0 | 0 - 1 | 1 |
| v0.15.0 | 0 - 1 | 1 |
| v0.14.2 | 0 - 1 | 1 |
| v0.14.1 | 0 - 1 | 1 |
| v0.14.0 | 0 - 1 | 1 |
| v0.13.3 | 0 - 1 | 1 |
| v0.13.2 | 0 - 1 | 1 |
| v0.13.1 | 0 - 1 | 1 |
| v0.13.0 | 0 - 1 | 1 |
| v0.12.2 | 0 - 1 | 1 |
| v0.12.1 | 0 - 1 | 1 |
| v0.12.0 | 0 - 1 | 1 |
| v0.11.0 | 0 - 1 | 1 |
| v0.10.0 | 0 - 1 | 1 |
| v0.9.0 | 0 - 1 | 1 |
| v0.8.0 | 0 - 1 | 1 |
| v0.7.2 | 0 - 1 | 1 |
| v0.7.1 | 0 - 1 | 1 |
| v0.7.0 | 0 - 1 | 1 |
