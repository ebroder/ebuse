# ebuse

ebuse is an [NBD][] server which uses the new EBS direct APIs to expose an EBS
snapshot as a network-accessible block device. You can use this to access data
in an EBS snapshot without needing to first restore it to an EBS volume.

This implementation is definitely a bit of a toy, so I probably wouldn't use it
for production workloads. There aren't any tests, it's not particularly hardened
against error conditions, and it doesn't try to be particularly efficient in
data that it fetches. I also can't promise that it will be particularly well
maintained.

Also, keep in mind that the EBS direct APIs aren't entirely free, although they
are quite cheap. Assume that you'll spend about 0.6c for every gigabyte you
read.

Why "ebuse"? Originally I planned to build this using [BUSE][] (well, a Go
version of BUSE). "ebuse" is what you get when you smash "EBS" and "BUSE"
together. It also seemed appropriate that it sounded abusive - this is probably
not exactly what these APIs are intended for.

## Usage

You can install ebuse by running

```
$ go get -u github.com/ebroder/ebuse
```

ebuse only has one required argument - the snapshot ID you want it to serve. It
optionally accepts as flags the AWS region of that snapshot (although that can
also be passed in via the `AWS_REGION` environment variable) and the UNIX socket
path to bind the NBD server to (which defaults to somewhere reasonable in
`XDG_RUNTIME_DIR`):

```
usage: ebuse [flags] snap-12345678
  -region string
    	AWS region of snapshot (default "us-east-1")
  -socket string
    	path to listen on (default "/run/user/1000/nbd.sock")
```

ebuse depends on having access to AWS credentials from the environment. It
supports everything that the AWS SDK supports [out of the box][AWS credentials].

Once you've started the ebuse NBD server, you can connect to it using something
like `nbd-client`:

```
$ sudo nbd-client -unix $XDG_RUNTIME_DIR/nbd.sock
Warning: the oldstyle protocol is no longer supported.
This method now uses the newstyle protocol with a default export
Negotiation: ..size = 8192MB
Connected /dev/nbd1
```

...and voila! You now have a block device (`/dev/nbd1` in this case) backed by
an EBS snapshot. Note that if you want to mount the block device, you'll need to
pass `-o ro` to `mount`; otherwise it will try and fail to update the
superblock.

To disconnect, run

```
sudo nbd-client -d /dev/nbd1
```

[NBD]: https://en.wikipedia.org/wiki/Network_block_device
[BUSE]: https://github.com/acozzette/BUSE
[AWS credentials]: https://docs.aws.amazon.com/sdk-for-go/api/#hdr-Configuring_Credentials
