#!/usr/bin/env -S gawk -f ${_} -- browser.go

/^var \(/ {
    var = 1
}

/^\)$/ && var {
    var = 0
}

var && match($0, /^\t(\w+)SelectorList/, sel_name) {
    sel_block = sel_name[1]
}

var && sel_block && match($0, /^\t\t`([^'`]+)`,$/, sel) {
    sel_joined = sel_joined ? sprintf("%s, %s", sel_joined, sel[1]) : sel[1]
}

var && /^\t}/ && sel_block {
    if (sel_block ~ /infoBox|nav[A-Z]/) {
        printf "function %s() { document.querySelectorAll('%s').forEach(e => e.%s); }\n",
            sel_block,
            sel_joined,
            sel_block ~ /navButton/ ? "style.visibility = 'hidden'" : "remove()"
    }
    sel_block = sel_joined = ""
}

END {
    print "infoBox(); navSection(); navButton();"
}
