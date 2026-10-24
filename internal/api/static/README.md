`make build` copies the web app (`web/dist/client`) and the widget loader
(`widget/dist/widget.js`) here so they are embedded into the binary. Without
them the server answers `/` with a placeholder page.
