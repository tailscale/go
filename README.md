# Tailscale's temporary Go fork

This is Tailscale's temporary fork of https://github.com/golang/go/.

We use this to build Tailscale releases when we need patches to Go
that aren't in a release yet.

For example, Tailscale on iOS is limited to 15 MiB of total disk+RAM
for our NetworkExtension, so we need to compile out parts of the Go
standard library to get our binary size down so we have any RAM left
over to work with.

This fork is not supported for use by others, and we don't accept
changes from others. It's our intention to upstream all of this into
Go itself.
