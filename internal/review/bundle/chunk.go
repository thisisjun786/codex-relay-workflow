package bundle

import "fmt"

func group(files []file, prefix string, cap int) ([]Chunk, error) {
	if len(prefix) > cap {
		return nil, fmt.Errorf("%w: preamble (%d > %d)", ErrSizeLimit, len(prefix), cap)
	}
	chunks := []Chunk{{Text: prefix}}
	if len(files) == 0 {
		chunks[0].Text += "(No changes detected)\n"
		if len(chunks[0].Text) > cap {
			return nil, fmt.Errorf("%w: empty bundle", ErrSizeLimit)
		}
		return chunks, nil
	}
	for _, f := range files {
		body := renderFile(f)
		if len(body) > cap-len(prefix) {
			return nil, fmt.Errorf("%w: file %q needs %d bytes plus %d preamble; cap %d", ErrSizeLimit, f.Path, len(body), len(prefix), cap)
		}
		last := len(chunks) - 1
		if len(body) > cap-len(chunks[last].Text) {
			chunks = append(chunks, Chunk{Text: prefix})
			last++
		}
		chunks[last].Text += body
		chunks[last].Paths = append(chunks[last].Paths, f.Path)
	}
	return chunks, nil
}
