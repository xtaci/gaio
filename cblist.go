package gaio

type cbList []*aiocb

func (l *cbList) PushBack(cb *aiocb) {
	*l = append(*l, cb)
}

func (l *cbList) Remove(cb *aiocb) {
	for idx, v := range *l {
		if v == cb {
			*l = append((*l)[:idx], (*l)[idx+1:]...)
			return
		}
	}
}

func (l *cbList) RemoveHeadN(n int) {
	*l = append((*l)[:0], (*l)[n:]...)
}
