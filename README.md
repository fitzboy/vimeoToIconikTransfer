# vimeoToIconikTransfer
script to copy videos from vimeo over to Iconik. It requires you to have a -vimeo-access-token as well as an -iconik-appid and -iconik-auth-token.
you also specify -iconik-collection which will be a base/root collection in iconik which the whole vimeo tree will be put under.

it will list all the folders in your vimeo account (and recurse into them) and then check to see if the sub-collections and files already exist (and their sizes match) with what is in vimeo, and if they don't exist already, it will create the sub-collection and copy the file (first locally, then) to iconik. as such, you can run the script and if needed, you can kill it and restart it later. you might have some dangling multi-part uploads along with an incomplete upload (the one that was in process of upload when you killed the script) but otherwise it is ok to stop and restart.

To Use:
- clone this repo locally
- install golang
- go build
- get iconik app-id and auth-token (from Tim)
- get Vimeo access-token (go to http://developer.vimeo.com and register an app, then manage authentication and get an access token)
- start it up in screen and pipe output to a log
