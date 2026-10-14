require("fs").appendFileSync(process.env.MARKERS, require("./package.json").name + "\n")
